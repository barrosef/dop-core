// Adaptador de SandboxLauncher sobre o Kubernetes.
//
// É a superfície de orquestração do produto nos DOIS modos — SaaS no cluster do
// DOP e infra do cliente — mudando kubeconfig e limites, não implementação
// (spec do substrato §2). Um namespace por demanda, um PVC com o workspace, um
// pod com o agente.
//
// Fala pela API do cluster, com a CA da service account, do mesmo jeito e pelo
// mesmo motivo que o adaptador de SecretStore: objeto lido por volume montado é
// eventualmente consistente, e aqui a leitura logo depois da escrita é o caso
// normal — provisionar e descrever acontecem na mesma requisição do usuário.
package sandbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// serviceAccountCA é onde o kubelet monta a CA do cluster em todo pod.
const k8sServiceAccountCA = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

// Nomes fixos dentro do namespace da demanda. Fixos de propósito: o namespace
// já isola, então o objeto não precisa de sufixo — e nome previsível é o que
// permite descrever um sandbox sabendo só o handle.
const (
	podName       = "sandbox"
	pvcName       = "workspace"
	containerName = "sandbox"
)

type K8s struct {
	client *http.Client
	stream *http.Client
	// tls é a MESMA configuração do transporte HTTP, guardada porque o exec
	// não passa por `http.Client`: ele disca o socket na mão para poder falar
	// WebSocket (ver websocket.go). Sem guardá-la, o exec confiaria só nas CAs
	// públicas e falharia contra o certificado da CA do próprio cluster.
	tls       *tls.Config
	apiServer string
	token     string
	// workspaceSize é o tamanho do PVC do workspace. Não está na porta: é
	// limite de implantação, e o Docker não tem o que fazer com ele.
	workspaceSize string
	storageClass  string
}

type K8sConfig struct {
	APIServer     string
	Token         string
	WorkspaceSize string
	StorageClass  string
	Client        *http.Client
	Timeout       time.Duration
}

func NewK8s(cfg K8sConfig) *K8s {
	c := cfg.Client
	if c == nil {
		to := cfg.Timeout
		if to <= 0 {
			to = 30 * time.Second
		}
		c = &http.Client{Timeout: to, Transport: k8sTransport()}
	}
	size := cfg.WorkspaceSize
	if size == "" {
		size = "10Gi"
	}
	return &K8s{
		client: c,
		// Timeout zero: seguir log dura o tempo do cliente.
		stream:        &http.Client{Transport: k8sTransport()},
		tls:           k8sTLS(),
		apiServer:     strings.TrimRight(cfg.APIServer, "/"),
		token:         cfg.Token,
		workspaceSize: size,
		storageClass:  cfg.StorageClass,
	}
}

var _ ports.SandboxLauncher = (*K8s)(nil)

// k8sTransport confia na CA do cluster ALÉM das públicas — mesma armadilha do
// adaptador de SecretStore: o certificado do apiserver é assinado pela CA do
// próprio cluster, que não está em bundle nenhum, e o pod sobe verde para
// quebrar só na primeira chamada de verdade.
func k8sTransport() *http.Transport {
	return &http.Transport{TLSClientConfig: k8sTLS()}
}

// k8sTLS existe separada porque DOIS caminhos precisam dela: o `http.Client` das
// chamadas normais e o socket discado na mão do exec. Duplicar a montagem seria
// a garantia de que um dos dois deixaria de confiar na CA do cluster no primeiro
// ajuste, e a falha apareceria só na primeira chamada de verdade.
func k8sTLS() *tls.Config {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if pem, err := os.ReadFile(k8sServiceAccountCA); err == nil {
		pool.AppendCertsFromPEM(pem)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func (k *K8s) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, errs.Wrap(errs.KindInternal, err, "pedido ilegível para o Kubernetes")
		}
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, k.apiServer+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, nil, errs.Wrap(errs.KindUnavailable, err, "falha ao falar com a API do Kubernetes")
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, errs.Wrap(errs.KindInternal, err, "resposta truncada do Kubernetes")
	}
	return resp.StatusCode, out, nil
}

// k8sFail preserva a mensagem do apiserver. É ela que diz "forbidden: cannot
// create resource pods" — a informação que separa cinco minutos de conserto de
// uma tarde de adivinhação.
func k8sFail(code int, body []byte, what string) error {
	var st struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &st)
	msg := strings.TrimSpace(st.Message)
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	switch code {
	case http.StatusNotFound:
		return errs.NotFound("%s", what)
	case http.StatusConflict:
		return errs.New(errs.KindAlreadyExists, "%s: %s", what, msg)
	case http.StatusForbidden, http.StatusUnauthorized:
		return errs.Permission("o Kubernetes negou %s: %s", what, msg)
	}
	return errs.Internal("o Kubernetes recusou %s (HTTP %d): %s", what, code, msg)
}

// ── tiers ────────────────────────────────────────────────────────────────────

// runtimeClasses lê o que o cluster oferece. É o R-4 da spec em uma chamada:
// RuntimeClass de Kata falta na maioria das distribuições, e a única resposta
// aceitável para isso é recusa com mensagem.
func (k *K8s) runtimeClasses(ctx context.Context) ([]string, error) {
	code, body, err := k.do(ctx, http.MethodGet, "/apis/node.k8s.io/v1/runtimeclasses", nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusForbidden || code == http.StatusUnauthorized {
		// Não dá para responder "só namespace" aqui: seria afirmar que o
		// cluster não tem Kata quando na verdade não sabemos olhar. Falta de
		// permissão é problema de instalação e precisa aparecer como tal.
		return nil, errs.Permission(
			"sem permissão para listar runtimeclasses.node.k8s.io — sem ela o " +
				"substrato não consegue provar qual isolamento oferece")
	}
	if code == http.StatusNotFound {
		return nil, nil // cluster sem a API de RuntimeClass: só isolamento por namespace
	}
	if code >= 300 {
		return nil, k8sFail(code, body, "listagem de runtimeclasses")
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "resposta ilegível do Kubernetes")
	}
	out := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, it.Metadata.Name)
	}
	sort.Strings(out)
	return out, nil
}

func (k *K8s) SupportedTiers(ctx context.Context) ([]ports.IsolationTier, error) {
	classes, err := k.runtimeClasses(ctx)
	if err != nil {
		return nil, err
	}
	// securityContext estrito não depende de RuntimeClass nenhuma: todo cluster
	// entrega isolamento por namespace.
	tiers := []ports.IsolationTier{ports.TierNamespace}
	if matchClass(classes, "kata", "firecracker") != "" {
		tiers = append(tiers, ports.TierHardware)
	}
	if matchClass(classes, "gvisor", "runsc", "edera") != "" {
		tiers = append(tiers, ports.TierKernelEmulated)
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i] < tiers[j] })
	return tiers, nil
}

func matchClass(classes []string, want ...string) string {
	for _, c := range classes {
		for _, w := range want {
			if strings.Contains(strings.ToLower(c), w) {
				return c
			}
		}
	}
	return ""
}

// runtimeClassFor devolve a RuntimeClass que ENTREGA o tier pedido, ou recusa.
// Nunca devolve "a mais parecida": é a linha entre isolamento declarado e
// isolamento presumido.
func (k *K8s) runtimeClassFor(ctx context.Context, tier ports.IsolationTier) (string, error) {
	if tier == ports.TierNamespace {
		return "", nil // sem RuntimeClass; o securityContext faz o trabalho
	}
	classes, err := k.runtimeClasses(ctx)
	if err != nil {
		return "", err
	}
	var found string
	switch tier {
	case ports.TierHardware:
		found = matchClass(classes, "kata", "firecracker")
	case ports.TierKernelEmulated:
		found = matchClass(classes, "gvisor", "runsc", "edera")
	}
	if found == "" {
		return "", errs.Precondition(
			"este cluster não tem RuntimeClass para isolamento %q — instale o "+
				"runtime correspondente ou peça outro nível (spec do substrato, R-4)", tier)
	}
	return found, nil
}

// ── ciclo de vida ────────────────────────────────────────────────────────────

func (k *K8s) Launch(ctx context.Context, spec ports.SandboxSpec) (*ports.SandboxStatus, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	// Resolvido ANTES de criar namespace: tier recusado não deixa rastro.
	runtimeClass, err := k.runtimeClassFor(ctx, spec.Tier)
	if err != nil {
		return nil, err
	}

	if st, err := k.Describe(ctx, spec.SandboxHandle); err == nil {
		return st, nil // relançar devolve o que existe (garantia 4)
	} else if errs.KindOf(err) != errs.KindNotFound {
		return nil, err
	}

	if err := k.ensureNamespace(ctx, spec); err != nil {
		return nil, err
	}
	if err := k.ensureWorkspace(ctx, spec); err != nil {
		return nil, err
	}
	if err := k.ensurePod(ctx, spec, runtimeClass); err != nil {
		return nil, err
	}
	return k.Describe(ctx, spec.SandboxHandle)
}

func (k *K8s) ensureNamespace(ctx context.Context, spec ports.SandboxSpec) error {
	labels := labelsFor(spec)
	code, body, err := k.do(ctx, http.MethodPost, "/api/v1/namespaces", map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": spec.Namespace, "labels": labels},
	})
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusConflict {
		return k8sFail(code, body, "criação do namespace da demanda")
	}
	return nil
}

// ensureWorkspace cria o PVC. É ELE que sobrevive à suspensão — o pod é
// descartável, o workspace não.
func (k *K8s) ensureWorkspace(ctx context.Context, spec ports.SandboxSpec) error {
	claim := map[string]any{
		"accessModes": []string{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]string{"storage": k.workspaceSize}},
	}
	if k.storageClass != "" {
		claim["storageClassName"] = k.storageClass
	}
	code, body, err := k.do(ctx, http.MethodPost,
		"/api/v1/namespaces/"+spec.Namespace+"/persistentvolumeclaims", map[string]any{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": map[string]any{"name": pvcName, "labels": labelsFor(spec)},
			"spec":     claim,
		})
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusConflict {
		return k8sFail(code, body, "criação do workspace")
	}
	return nil
}

func (k *K8s) ensurePod(ctx context.Context, spec ports.SandboxSpec, runtimeClass string) error {
	env := make([]map[string]string, 0, len(spec.Env))
	keys := make([]string, 0, len(spec.Env))
	for key := range spec.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, map[string]string{"name": key, "value": spec.Env[key]})
	}

	container := map[string]any{
		"name":  containerName,
		"image": spec.Image,
		"env":   env,
		// O `pods/exec` NÃO aceita diretório de trabalho — só o comando. Fixar
		// o do contêiner é o que faz o exec herdá-lo e o que torna a garantia
		// 14 da porta a mesma nos dois substratos: comando de ferramenta
		// começa no workspace, aqui e no Docker.
		"workingDir": ports.SandboxWorkspacePath,
		"volumeMounts": []map[string]any{
			{"name": pvcName, "mountPath": ports.SandboxWorkspacePath},
		},
		// Defesa em camadas (spec §6): o agente lê conteúdo não confiável e
		// porta credencial. Sem capacidade, sem reescalar privilégio.
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false,
			"capabilities":             map[string]any{"drop": []string{"ALL"}},
		},
	}
	if len(spec.Command) > 0 {
		container["command"] = spec.Command
	}

	podSpec := map[string]any{
		// Never: um sandbox que morre em laço precisa APARECER como parado. Com
		// reinício automático o domínio veria "ativo" para sempre e o dev
		// ficaria olhando um terminal que reinicia sozinho.
		"restartPolicy": "Never",
		"containers":    []any{container},
		"volumes": []map[string]any{
			{"name": pvcName, "persistentVolumeClaim": map[string]string{"claimName": pvcName}},
		},
		// Usuário arbitrário NÃO-root desde a primeira imagem: OKD e OpenShift
		// recusam root por SCC, e isso é requisito de imagem, não de
		// implantação (spec §2). fsGroup é o que deixa o workspace gravável
		// para esse usuário.
		"securityContext": map[string]any{
			"runAsNonRoot":   true,
			"runAsUser":      1000,
			"runAsGroup":     1000,
			"fsGroup":        1000,
			"seccompProfile": map[string]string{"type": "RuntimeDefault"},
		},
	}
	if runtimeClass != "" {
		podSpec["runtimeClassName"] = runtimeClass
	}

	code, body, err := k.do(ctx, http.MethodPost,
		"/api/v1/namespaces/"+spec.Namespace+"/pods", map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": podName, "labels": labelsFor(spec)},
			"spec":     podSpec,
		})
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusConflict {
		return k8sFail(code, body, "criação do pod do sandbox")
	}
	return nil
}

// Suspend APAGA o pod e deixa o PVC.
//
// É a divergência que a suíte de contrato tornou explícita: aqui a suspensão
// leva tudo o que não estava no PVC — inclusive o log do pod —, enquanto o
// adaptador do Docker apenas para o contêiner e conserva a camada gravável.
// Por isso a porta promete APENAS o que está sob SandboxWorkspacePath.
func (k *K8s) Suspend(ctx context.Context, h ports.SandboxHandle) error {
	if _, err := k.Describe(ctx, h); err != nil {
		return err
	}
	code, body, err := k.do(ctx, http.MethodDelete,
		"/api/v1/namespaces/"+h.Namespace+"/pods/"+podName+"?gracePeriodSeconds=10", nil)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusNotFound {
		return k8sFail(code, body, "suspensão do sandbox")
	}
	return nil
}

// Resume recria o pod SOBRE o PVC existente.
func (k *K8s) Resume(ctx context.Context, spec ports.SandboxSpec) (*ports.SandboxStatus, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	st, err := k.Describe(ctx, spec.SandboxHandle)
	if err != nil {
		return nil, err
	}
	if st.Phase == ports.PhaseActive {
		return st, nil
	}
	runtimeClass, err := k.runtimeClassFor(ctx, spec.Tier)
	if err != nil {
		return nil, err
	}
	// Um pod parado (Succeeded/Failed) não "reinicia": some e volta. Apagar
	// antes é o que torna Resume idempotente de verdade.
	if err := k.deletePodAndWait(ctx, spec.Namespace); err != nil {
		return nil, err
	}
	if err := k.ensurePod(ctx, spec, runtimeClass); err != nil {
		return nil, err
	}
	return k.Describe(ctx, spec.SandboxHandle)
}

// deletePodAndWait espera o pod sumir de verdade. Sem a espera, o POST logo
// depois bate em 409 com o pod ainda terminando e o Resume falharia de vez em
// quando — o pior tipo de defeito, o que só aparece na máquina dos outros.
func (k *K8s) deletePodAndWait(ctx context.Context, ns string) error {
	code, body, err := k.do(ctx, http.MethodDelete,
		"/api/v1/namespaces/"+ns+"/pods/"+podName+"?gracePeriodSeconds=0", nil)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusNotFound {
		return k8sFail(code, body, "remoção do pod anterior")
	}
	for i := 0; i < 60; i++ {
		code, _, err := k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods/"+podName, nil)
		if err != nil {
			return err
		}
		if code == http.StatusNotFound {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errs.New(errs.KindUnavailable, "o pod anterior do sandbox não terminou a tempo")
}

// Destroy apaga o NAMESPACE inteiro: pod, PVC e tudo o que a demanda tenha
// criado dentro dele. É o que torna a destruição irreversível de verdade —
// apagar objeto por objeto deixaria para trás o que ninguém previu.
func (k *K8s) Destroy(ctx context.Context, h ports.SandboxHandle) error {
	code, body, err := k.do(ctx, http.MethodDelete, "/api/v1/namespaces/"+h.Namespace, nil)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusNotFound && code != http.StatusConflict {
		return k8sFail(code, body, "destruição do sandbox")
	}
	return nil
}

func (k *K8s) Describe(ctx context.Context, h ports.SandboxHandle) (*ports.SandboxStatus, error) {
	nsTier, err := k.namespaceTier(ctx, h)
	if err != nil {
		return nil, err
	}

	code, body, err := k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+h.Namespace+"/pods/"+podName, nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		// Sem pod, com PVC: suspenso. Sem os dois: o namespace existe mas não é
		// um sandbox nosso — para a porta, não existe.
		ok, err := k.workspaceExists(ctx, h)
		if err != nil {
			return nil, err
		}
		if ok {
			return &ports.SandboxStatus{Phase: ports.PhaseSuspended, Tier: nsTier}, nil
		}
		return nil, errs.NotFound("sandbox %s", h.ID)
	}
	if code >= 300 {
		return nil, k8sFail(code, body, "leitura do sandbox")
	}

	var pod struct {
		Metadata struct {
			Labels            map[string]string `json:"labels"`
			DeletionTimestamp *string           `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Ports []struct {
					Name          string `json:"name"`
					ContainerPort int32  `json:"containerPort"`
				} `json:"ports"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &pod); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "resposta ilegível do Kubernetes")
	}

	tier := ports.IsolationTier(pod.Metadata.Labels[labelTier])
	if tier == ports.TierUnspecified {
		tier = nsTier
	}
	st := &ports.SandboxStatus{Tier: tier}
	switch pod.Status.Phase {
	case "Running":
		st.Phase = ports.PhaseActive
	case "Pending":
		st.Phase = ports.PhaseProvisioning
	default: // Succeeded, Failed, Unknown — a execução acabou, o PVC continua
		st.Phase = ports.PhaseSuspended
	}
	if pod.Metadata.DeletionTimestamp != nil {
		st.Phase = ports.PhaseSuspended
	}

	running := st.Phase == ports.PhaseActive
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			name := p.Name
			if name == "" {
				name = fmt.Sprintf("port-%d", p.ContainerPort)
			}
			state := "stopped"
			if running {
				state = "running"
			}
			st.Endpoints = append(st.Endpoints, ports.SandboxEndpoint{
				Name: name, Port: p.ContainerPort, State: state,
			})
		}
	}
	return st, nil
}

// namespaceTier lê o tier da LABEL do namespace, que sobrevive à suspensão.
// Namespace ausente — ou já em Terminating — é sandbox inexistente: um
// namespace que está sumindo não volta, e tratá-lo como vivo faria Destroy
// parecer não ter funcionado.
func (k *K8s) namespaceTier(ctx context.Context, h ports.SandboxHandle) (ports.IsolationTier, error) {
	code, body, err := k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+h.Namespace, nil)
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		return "", errs.NotFound("sandbox %s", h.ID)
	}
	if code >= 300 {
		return "", k8sFail(code, body, "leitura do namespace da demanda")
	}
	var ns struct {
		Metadata struct {
			Labels            map[string]string `json:"labels"`
			DeletionTimestamp *string           `json:"deletionTimestamp"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &ns); err != nil {
		return "", errs.Wrap(errs.KindInternal, err, "resposta ilegível do Kubernetes")
	}
	if ns.Metadata.DeletionTimestamp != nil {
		return "", errs.NotFound("sandbox %s", h.ID)
	}
	return ports.IsolationTier(ns.Metadata.Labels[labelTier]), nil
}

func (k *K8s) workspaceExists(ctx context.Context, h ports.SandboxHandle) (bool, error) {
	code, body, err := k.do(ctx, http.MethodGet,
		"/api/v1/namespaces/"+h.Namespace+"/persistentvolumeclaims/"+pvcName, nil)
	if err != nil {
		return false, err
	}
	if code == http.StatusNotFound {
		return false, nil
	}
	if code >= 300 {
		return false, k8sFail(code, body, "leitura do workspace")
	}
	return true, nil
}

// ── exec ─────────────────────────────────────────────────────────────────────

// wsProtocoloExec é o subprotocolo de canais do Kubernetes.
//
// `v4` e não `v5`: o v5 acrescenta só o fechamento de stdin, e este exec não tem
// stdin. Pedir a versão maior sem precisar dela seria trocar compatibilidade com
// cluster antigo por nada.
const wsProtocoloExec = "v4.channel.k8s.io"

// Os canais do subprotocolo. O 0 (stdin) e o 4 (resize) não são usados aqui —
// é justamente o que separa "rodar um comando" de "abrir uma sessão".
const (
	canalStdout = 1
	canalStderr = 2
	canalStatus = 3
)

// Exec roda um comando dentro do contêiner do sandbox (garantias 13 a 18).
//
// O código de saída é o ponto delicado deste adaptador. Ele NÃO vem por HTTP e
// NÃO vem no fim do stdout: vem no canal 3, como um `metav1.Status` em JSON, e
// só ali. Um exec que leia stdout/stderr e feche a conexão funciona
// perfeitamente e devolve o código de saída errado (zero) para todo comando que
// falhou — que é exatamente a confusão que a garantia 15 existe para impedir.
func (k *K8s) Exec(ctx context.Context, h ports.SandboxHandle, req ports.ExecRequest) (*ports.ExecResult, error) {
	if len(req.Command) == 0 {
		return nil, errs.Invalid("exec sem comando")
	}
	st, err := k.Describe(ctx, h)
	if err != nil {
		return nil, err
	}
	if st.Phase != ports.PhaseActive {
		return nil, errs.Precondition(
			"o sandbox %s está em %q e não executa comando; retome-o antes", h.ID, st.Phase)
	}

	prazo, teto := execLimites(req)
	runCtx, cancel := context.WithTimeout(ctx, prazo)
	defer cancel()

	v := url.Values{}
	v.Set("container", containerName)
	v.Set("stdout", "true")
	v.Set("stderr", "true")
	// stdin DESLIGADO: ver o cabeçalho de websocket.go. Comando, não sessão.
	v.Set("stdin", "false")
	v.Set("tty", "false")
	for _, arg := range req.Command {
		v.Add("command", arg)
	}
	endereco := k.apiServer + "/api/v1/namespaces/" + h.Namespace + "/pods/" + podName +
		"/exec?" + v.Encode()

	cab := http.Header{}
	if k.token != "" {
		cab.Set("Authorization", "Bearer "+k.token)
	}
	ws, resp, err := wsDial(runCtx, endereco, cab, wsProtocoloExec, k.tls)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errs.Wrap(errs.KindUnavailable, ctx.Err(), "execução interrompida pelo chamador")
		}
		if runCtx.Err() != nil {
			// Nem o aperto de mão coube no prazo. Continua sendo RESULTADO:
			// quem pediu um prazo curto precisa ver "não coube", não um erro
			// de infraestrutura que manda investigar o cluster.
			return &ports.ExecResult{ExitCode: -1, TimedOut: true}, nil
		}
		return nil, err
	}
	if ws == nil {
		corpo, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden &&
			strings.Contains(strings.ToLower(string(corpo)), "reject") {
			// O `kubectl proxy` recusa `/pods/*/exec` por padrão. A mensagem
			// crua dele não diz o que fazer; esta diz.
			return nil, errs.Permission(
				"o proxy à frente da API recusou o caminho de exec — rode " +
					"`kubectl proxy --port=8001 --reject-paths='^$'` ou aponte " +
					"K8S_API_SERVER direto para o apiserver")
		}
		return nil, k8sFail(resp.StatusCode, corpo, "execução do comando no sandbox")
	}
	defer ws.Close()

	saida, erroPadrao := &bufferComTeto{max: teto}, &bufferComTeto{max: teto}
	var status []byte
	for {
		msg, err := ws.ReadMessage()
		if err != nil {
			break // EOF, fechamento ou socket derrubado pelo prazo
		}
		if len(msg) == 0 {
			// O k8s abre cada canal com uma mensagem só com o byte do canal.
			continue
		}
		dados := msg[1:]
		switch msg[0] {
		case canalStdout:
			_, _ = saida.Write(dados)
		case canalStderr:
			_, _ = erroPadrao.Write(dados)
		case canalStatus:
			status = append(status, dados...)
		}
	}

	if ctx.Err() != nil {
		return nil, errs.Wrap(errs.KindUnavailable, ctx.Err(), "execução interrompida pelo chamador")
	}
	res := &ports.ExecResult{
		ExitCode:  -1,
		Stdout:    saida.String(),
		Stderr:    erroPadrao.String(),
		Truncated: saida.cortou || erroPadrao.cortou,
		TimedOut:  runCtx.Err() != nil,
	}
	if res.TimedOut {
		// Sem status no canal 3, não houve término: o processo continua lá
		// dentro. -1 e TimedOut dizem isso; zero afirmaria sucesso.
		return res, nil
	}
	aplicarStatusDeExec(res, status)
	return res, nil
}

// aplicarStatusDeExec lê o `metav1.Status` do canal 3.
//
// Três casos, e os três são RESULTADO, nunca erro da porta:
//
//   - Success → código 0;
//   - Failure com causa ExitCode → o código que o processo devolveu;
//   - Failure sem causa de código (o clássico "executable file not found") → -1,
//     e a mensagem do substrato entra no stderr. Ela precisa chegar ao MODELO,
//     que é quem consegue corrigir o comando; engoli-la deixaria o agente com
//     uma saída vazia e nenhuma pista.
func aplicarStatusDeExec(res *ports.ExecResult, bruto []byte) {
	if len(bruto) == 0 {
		return
	}
	var st struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
		Details struct {
			Causes []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"causes"`
		} `json:"details"`
	}
	if err := json.Unmarshal(bruto, &st); err != nil {
		return
	}
	if st.Status == "Success" {
		res.ExitCode = 0
		return
	}
	for _, c := range st.Details.Causes {
		if c.Reason == "ExitCode" {
			if n, err := strconv.Atoi(strings.TrimSpace(c.Message)); err == nil {
				res.ExitCode = n
				return
			}
		}
	}
	if msg := strings.TrimSpace(st.Message); msg != "" {
		if res.Stderr != "" && !strings.HasSuffix(res.Stderr, "\n") {
			res.Stderr += "\n"
		}
		res.Stderr += msg
	}
}

// ── logs ─────────────────────────────────────────────────────────────────────

// Tail segue o log do pod e morre junto com o chamador.
//
// O k8s entrega texto puro, uma linha por linha, com carimbo RFC3339 quando se
// pede. Não há multiplexação: stdout e stderr chegam FUNDIDOS. É a razão de a
// porta não prometer nada sobre LogLine.Stream — o Docker separa, este não, e
// prometer o que só um cumpre é a abstração vazando.
func (k *K8s) Tail(ctx context.Context, h ports.SandboxHandle, q ports.LogQuery, emit func(ports.LogLine) error) error {
	if _, err := k.Describe(ctx, h); err != nil {
		return err
	}
	container := containerName
	if q.Service != "" {
		if q.Service != containerName {
			return errs.NotFound("processo %q no sandbox %s", q.Service, h.ID)
		}
		container = q.Service
	}

	v := url.Values{}
	v.Set("container", container)
	v.Set("timestamps", "true")
	if q.Follow {
		v.Set("follow", "true")
	}
	if q.TailLines > 0 {
		v.Set("tailLines", fmt.Sprintf("%d", q.TailLines))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		k.apiServer+"/api/v1/namespaces/"+h.Namespace+"/pods/"+podName+"/log?"+v.Encode(), nil)
	if err != nil {
		return err
	}
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
	resp, err := k.stream.Do(req)
	if err != nil {
		if ctxEnded(ctx) {
			return nil
		}
		return errs.Wrap(errs.KindUnavailable, err, "falha ao seguir os logs")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		out, _ := io.ReadAll(resp.Body)
		return k8sFail(resp.StatusCode, out, "leitura dos logs")
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if ctxEnded(ctx) {
			return nil
		}
		at, text := splitTimestamp(sc.Text())
		if err := emit(ports.LogLine{Service: containerName, Stream: "stdout", Text: text, At: at}); err != nil {
			return err
		}
	}
	return streamEnd(ctx, sc.Err())
}
