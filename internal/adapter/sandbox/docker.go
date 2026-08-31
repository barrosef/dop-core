// Adaptador de SandboxLauncher sobre o Docker do host.
//
// É ele que faz o desenvolvimento da plataforma existir sem cluster (spec do
// substrato §2) — e, pela ADR-0001, é a PROVA de que a porta está certa: com um
// adaptador só, ela sairia no formato do Kubernetes e ninguém notaria.
//
// Fala a Engine API pelo socket unix, com net/http — sem SDK. Não é
// preciosismo: o SDK do Docker arrasta a árvore de dependências do daemon
// inteiro para dentro de um binário que só precisa de sete chamadas HTTP.
package sandbox

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// DefaultDockerSocket é onde o daemon escuta em Linux.
const DefaultDockerSocket = "/var/run/docker.sock"

type Docker struct {
	client *http.Client
	// stream tem Timeout zero: um follow de log dura o tempo do cliente, e um
	// timeout no cliente HTTP mataria o tail no meio sem erro nenhum útil.
	stream  *http.Client
	apiVer  string
	timeout time.Duration
}

type DockerConfig struct {
	Socket string
	// APIVersion fixa a versão negociada. Sem ela o daemon usa a mais recente
	// que conhece — e uma atualização do host mudaria o formato de resposta
	// debaixo de nós.
	APIVersion string
	Timeout    time.Duration
}

func NewDocker(cfg DockerConfig) *Docker {
	socket := cfg.Socket
	if socket == "" {
		socket = DefaultDockerSocket
	}
	if v := os.Getenv("DOCKER_HOST"); v != "" && strings.HasPrefix(v, "unix://") {
		socket = strings.TrimPrefix(v, "unix://")
	}
	ver := cfg.APIVersion
	if ver == "" {
		ver = "v1.43"
	}
	to := cfg.Timeout
	if to <= 0 {
		to = 30 * time.Second
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	return &Docker{
		client:  &http.Client{Transport: &http.Transport{DialContext: dial}, Timeout: to},
		stream:  &http.Client{Transport: &http.Transport{DialContext: dial}},
		apiVer:  ver,
		timeout: to,
	}
}

var _ ports.SandboxLauncher = (*Docker)(nil)

// ── nomes ────────────────────────────────────────────────────────────────────
//
// O namespace da demanda vira PREFIXO de nome no Docker, porque o Docker não
// tem namespace. É a mesma identificação hierárquica da spec, expressa no que
// este substrato oferece — e por isso as labels vão junto: nome é para achar,
// label é para consultar.

func (d *Docker) containerName(h ports.SandboxHandle) string { return h.Namespace + "-sandbox" }
func (d *Docker) volumeName(h ports.SandboxHandle) string    { return h.Namespace + "-workspace" }

// ── HTTP ─────────────────────────────────────────────────────────────────────

func (d *Docker) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, errs.Wrap(errs.KindInternal, err, "pedido ilegível para o Docker")
		}
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/"+d.apiVer+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, errs.Wrap(errs.KindUnavailable, err, "falha ao falar com o Docker")
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, errs.Wrap(errs.KindInternal, err, "resposta truncada do Docker")
	}
	return resp.StatusCode, out, nil
}

// fail transforma o corpo de erro do Docker em erro de domínio com a mensagem
// que o daemon deu. Engolir essa mensagem é o que transforma "no such image"
// em "erro interno" e queima uma hora de investigação.
func fail(code int, body []byte, what string) error {
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	switch code {
	case http.StatusNotFound:
		return errs.NotFound("%s", what)
	case http.StatusConflict:
		return errs.New(errs.KindAlreadyExists, "%s: %s", what, msg)
	}
	return errs.Internal("o Docker recusou %s (HTTP %d): %s", what, code, msg)
}

// ── tiers ────────────────────────────────────────────────────────────────────

// SupportedTiers pergunta ao daemon quais runtimes ele tem registrados.
//
// É o mesmo raciocínio do adaptador k8s com RuntimeClass, do outro lado da
// porta: o nível de isolamento é um FATO do host, verificável, e não uma
// suposição da configuração. Host sem runsc e sem kata oferece exatamente
// `namespace` — e dizer isso em voz alta é o que permite ao domínio recusar em
// vez de degradar.
func (d *Docker) SupportedTiers(ctx context.Context) ([]ports.IsolationTier, error) {
	code, body, err := d.do(ctx, http.MethodGet, "/info", nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, fail(code, body, "consulta ao daemon")
	}
	var info struct {
		Runtimes map[string]any `json:"Runtimes"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "resposta ilegível do Docker")
	}
	tiers := map[ports.IsolationTier]bool{ports.TierNamespace: true} // runc sempre há
	for name := range info.Runtimes {
		switch {
		case strings.Contains(name, "kata"), strings.Contains(name, "firecracker"):
			tiers[ports.TierHardware] = true
		case strings.Contains(name, "runsc"), strings.Contains(name, "gvisor"),
			strings.Contains(name, "edera"):
			tiers[ports.TierKernelEmulated] = true
		}
	}
	out := make([]ports.IsolationTier, 0, len(tiers))
	for t := range tiers {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// runtimeFor devolve o runtime do Docker que entrega o tier pedido.
func (d *Docker) runtimeFor(ctx context.Context, tier ports.IsolationTier) (string, error) {
	code, body, err := d.do(ctx, http.MethodGet, "/info", nil)
	if err != nil {
		return "", err
	}
	if code >= 300 {
		return "", fail(code, body, "consulta ao daemon")
	}
	var info struct {
		Runtimes       map[string]any `json:"Runtimes"`
		DefaultRuntime string         `json:"DefaultRuntime"`
	}
	_ = json.Unmarshal(body, &info)

	want := func(match ...string) string {
		names := make([]string, 0, len(info.Runtimes))
		for n := range info.Runtimes {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			for _, m := range match {
				if strings.Contains(n, m) {
					return n
				}
			}
		}
		return ""
	}

	switch tier {
	case ports.TierNamespace:
		if info.DefaultRuntime != "" {
			return info.DefaultRuntime, nil
		}
		return "runc", nil
	case ports.TierKernelEmulated:
		if r := want("runsc", "gvisor", "edera"); r != "" {
			return r, nil
		}
	case ports.TierHardware:
		if r := want("kata", "firecracker"); r != "" {
			return r, nil
		}
	}
	// Garantia 1 da porta: recusa com mensagem, nunca um nível a menos.
	return "", errs.Precondition(
		"o Docker deste host não tem runtime para isolamento %q — instale e "+
			"registre o runtime correspondente ou peça outro nível", tier)
}

// ── ciclo de vida ────────────────────────────────────────────────────────────

func (d *Docker) Launch(ctx context.Context, spec ports.SandboxSpec) (*ports.SandboxStatus, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	// O runtime é resolvido ANTES de criar volume ou contêiner: tier recusado
	// não pode deixar rastro (garantia 2).
	runtime, err := d.runtimeFor(ctx, spec.Tier)
	if err != nil {
		return nil, err
	}

	// Relançar a mesma spec devolve o que já existe (garantia 4).
	if st, err := d.Describe(ctx, spec.SandboxHandle); err == nil {
		return st, nil
	} else if errs.KindOf(err) != errs.KindNotFound {
		return nil, err
	}

	if err := d.ensureVolume(ctx, spec); err != nil {
		return nil, err
	}
	if err := d.ensureImage(ctx, spec.Image); err != nil {
		return nil, err
	}
	if err := d.createContainer(ctx, spec, runtime); err != nil {
		return nil, err
	}
	if err := d.startContainer(ctx, spec.SandboxHandle); err != nil {
		return nil, err
	}
	return d.Describe(ctx, spec.SandboxHandle)
}

func (d *Docker) ensureVolume(ctx context.Context, spec ports.SandboxSpec) error {
	code, body, err := d.do(ctx, http.MethodPost, "/volumes/create", map[string]any{
		"Name":   d.volumeName(spec.SandboxHandle),
		"Labels": labelsFor(spec),
	})
	if err != nil {
		return err
	}
	// O Docker devolve 201 tanto na criação quanto quando o volume já existe.
	if code >= 300 {
		return fail(code, body, "criação do workspace")
	}
	return nil
}

// ensureImage puxa a imagem se ela não estiver no host.
//
// Sem isso, o primeiro Launch numa máquina limpa falha com "no such image" —
// e a suíte de contrato dependeria de alguém ter rodado docker pull antes,
// que é a definição de teste que passa por acidente.
func (d *Docker) ensureImage(ctx context.Context, image string) error {
	code, _, err := d.do(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", nil)
	if err != nil {
		return err
	}
	if code == http.StatusOK {
		return nil
	}
	// O pull pode demorar bem mais que uma chamada normal.
	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(pullCtx, http.MethodPost,
		"http://docker/"+d.apiVer+"/images/create?fromImage="+url.QueryEscape(image), nil)
	if err != nil {
		return err
	}
	resp, err := d.stream.Do(req)
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "falha ao baixar a imagem %s", image)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body) // o corpo é o progresso; precisa ser drenado
	if resp.StatusCode >= 300 {
		return fail(resp.StatusCode, out, "download da imagem "+image)
	}
	return nil
}

func (d *Docker) createContainer(ctx context.Context, spec ports.SandboxSpec, runtime string) error {
	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env) // criação reprodutível

	body := map[string]any{
		"Image":  spec.Image,
		"Env":    env,
		"Labels": labelsFor(spec),
		// O diretório de trabalho do CONTÊINER é o workspace, e é daqui que
		// sai a garantia 14 da porta: o `pods/exec` do k8s não aceita
		// diretório de trabalho, então em vez de emular no adaptador os dois
		// fixam o do contêiner e deixam o exec herdá-lo. Um comando de
		// ferramenta começa no mesmo lugar nos dois substratos.
		"WorkingDir": ports.SandboxWorkspacePath,
		// Tty falso mantém stdout e stderr SEPARADOS no stream de log. Com tty
		// os dois se fundem e LogLine.Stream passaria a mentir.
		"Tty": false,
		"HostConfig": map[string]any{
			"Runtime": runtime,
			"Binds":   []string{d.volumeName(spec.SandboxHandle) + ":" + ports.SandboxWorkspacePath},
			// Defesa em camadas (spec §6). O agente lê conteúdo não confiável e
			// porta credencial: capacidade nenhuma, e nada de reescalar.
			"CapDrop":     []string{"ALL"},
			"SecurityOpt": []string{"no-new-privileges:true"},
			// Reinício automático esconderia um sandbox que morre em laço: o
			// domínio precisa VER o estado, não um contêiner ressuscitando.
			"RestartPolicy": map[string]any{"Name": "no"},
		},
	}
	if len(spec.Command) > 0 {
		body["Cmd"] = spec.Command
	}

	code, resp, err := d.do(ctx, http.MethodPost,
		"/containers/create?name="+url.QueryEscape(d.containerName(spec.SandboxHandle)), body)
	if err != nil {
		return err
	}
	if code == http.StatusConflict {
		return nil // já existe: o Launch é idempotente
	}
	if code >= 300 {
		return fail(code, resp, "criação do sandbox")
	}
	return nil
}

func (d *Docker) startContainer(ctx context.Context, h ports.SandboxHandle) error {
	code, body, err := d.do(ctx, http.MethodPost,
		"/containers/"+d.containerName(h)+"/start", nil)
	if err != nil {
		return err
	}
	// 304 = já estava rodando. É sucesso, não erro.
	if code >= 300 && code != http.StatusNotModified {
		return fail(code, body, "início do sandbox")
	}
	return nil
}

// Suspend para a execução e PRESERVA o volume do workspace.
//
// Aqui mora a divergência mais instrutiva entre os dois adaptadores: o k8s
// apaga o pod e só o PVC sobrevive; o Docker para o contêiner e a camada
// gravável dele fica de pé. Os dois cumprem a garantia 5 — o que está sob
// /workspace sobrevive —, e apenas ela. Se a porta prometesse "o sandbox
// inteiro sobrevive", este adaptador cumpriria e o outro não, e a suíte de
// contrato existiria só para dar aval a uma mentira.
func (d *Docker) Suspend(ctx context.Context, h ports.SandboxHandle) error {
	if _, err := d.Describe(ctx, h); err != nil {
		return err // inexistente devolve NotFound (garantia 9)
	}
	code, body, err := d.do(ctx, http.MethodPost, "/containers/"+d.containerName(h)+"/stop?t=10", nil)
	if err != nil {
		return err
	}
	// 304 = já parado; 404 = contêiner já removido, mas o workspace está lá.
	if code >= 300 && code != http.StatusNotModified && code != http.StatusNotFound {
		return fail(code, body, "suspensão do sandbox")
	}
	return nil
}

func (d *Docker) Resume(ctx context.Context, spec ports.SandboxSpec) (*ports.SandboxStatus, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	st, err := d.Describe(ctx, spec.SandboxHandle)
	if err != nil {
		return nil, err
	}
	if st.Phase == ports.PhaseActive {
		return st, nil // idempotente (garantia 7)
	}
	// O contêiner pode ter sido removido com o volume intacto; nesse caso
	// recriar é o caminho de volta ao workspace existente.
	if code, _, err := d.do(ctx, http.MethodGet, "/containers/"+d.containerName(spec.SandboxHandle)+"/json", nil); err != nil {
		return nil, err
	} else if code == http.StatusNotFound {
		runtime, err := d.runtimeFor(ctx, spec.Tier)
		if err != nil {
			return nil, err
		}
		if err := d.createContainer(ctx, spec, runtime); err != nil {
			return nil, err
		}
	}
	if err := d.startContainer(ctx, spec.SandboxHandle); err != nil {
		return nil, err
	}
	return d.Describe(ctx, spec.SandboxHandle)
}

// Destroy leva execução E workspace. É irreversível por construção: depois do
// volume removido não há o que retomar.
func (d *Docker) Destroy(ctx context.Context, h ports.SandboxHandle) error {
	code, body, err := d.do(ctx, http.MethodDelete,
		"/containers/"+d.containerName(h)+"?force=1&v=0", nil)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusNotFound {
		return fail(code, body, "remoção do sandbox")
	}
	code, body, err = d.do(ctx, http.MethodDelete, "/volumes/"+d.volumeName(h)+"?force=1", nil)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusNotFound {
		return fail(code, body, "remoção do workspace")
	}
	return nil // ausência é o resultado desejado (garantia 8)
}

func (d *Docker) Describe(ctx context.Context, h ports.SandboxHandle) (*ports.SandboxStatus, error) {
	code, body, err := d.do(ctx, http.MethodGet, "/containers/"+d.containerName(h)+"/json", nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		// Sem contêiner, mas com workspace: suspenso. Sem os dois: não existe.
		// O tier vem da label do VOLUME, que sobrevive à suspensão — do mesmo
		// jeito que o adaptador k8s o lê da label do namespace. Sem isso,
		// retomar um sandbox suspenso não teria como reconferir o isolamento
		// declarado, e a degradação silenciosa entraria pela porta dos fundos.
		tier, ok, err := d.volumeTier(ctx, h)
		if err != nil {
			return nil, err
		}
		if ok {
			return &ports.SandboxStatus{Phase: ports.PhaseSuspended, Tier: tier}, nil
		}
		return nil, errs.NotFound("sandbox %s", h.ID)
	}
	if code >= 300 {
		return nil, fail(code, body, "leitura do sandbox")
	}

	var insp struct {
		State struct {
			Running bool   `json:"Running"`
			Status  string `json:"Status"`
		} `json:"State"`
		Config struct {
			Labels       map[string]string `json:"Labels"`
			ExposedPorts map[string]any    `json:"ExposedPorts"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(body, &insp); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "resposta ilegível do Docker")
	}

	st := &ports.SandboxStatus{
		Tier:      ports.IsolationTier(insp.Config.Labels[labelTier]),
		Endpoints: endpointsFromPorts(insp.Config.ExposedPorts, insp.State.Running),
	}
	switch {
	case insp.State.Running:
		st.Phase = ports.PhaseActive
	case insp.State.Status == "created":
		st.Phase = ports.PhaseProvisioning
	default:
		st.Phase = ports.PhaseSuspended
	}
	return st, nil
}

func (d *Docker) volumeTier(ctx context.Context, h ports.SandboxHandle) (ports.IsolationTier, bool, error) {
	code, body, err := d.do(ctx, http.MethodGet, "/volumes/"+d.volumeName(h), nil)
	if err != nil {
		return "", false, err
	}
	if code == http.StatusNotFound {
		return "", false, nil
	}
	if code >= 300 {
		return "", false, fail(code, body, "leitura do workspace")
	}
	var vol struct {
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(body, &vol); err != nil {
		return "", false, errs.Wrap(errs.KindInternal, err, "resposta ilegível do Docker")
	}
	return ports.IsolationTier(vol.Labels[labelTier]), true, nil
}

// ── exec ─────────────────────────────────────────────────────────────────────

// Exec roda um comando dentro do contêiner do sandbox (garantias 13 a 18).
//
// São TRÊS chamadas, e a terceira é a que muita implementação esquece:
// `/exec/create` monta o processo, `/exec/start` devolve o stream com a saída, e
// `/exec/{id}/json` é o ÚNICO lugar onde o código de saída aparece. Ler só o
// stream entregaria a saída de um comando que falhou com um código de saída
// zero inventado — que é exatamente a confusão que a garantia 15 existe para
// impedir.
func (d *Docker) Exec(ctx context.Context, h ports.SandboxHandle, req ports.ExecRequest) (*ports.ExecResult, error) {
	if len(req.Command) == 0 {
		return nil, errs.Invalid("exec sem comando")
	}
	// Fase ANTES de tentar: o Docker responde 409 para contêiner parado, e 409
	// é "já existe" no tradutor de erro deste adaptador. Perguntar primeiro dá
	// a mesma resposta do k8s — NotFound para inexistente, Precondition para
	// suspenso (garantia 18) — em vez de deixar cada substrato escolher a sua.
	st, err := d.Describe(ctx, h)
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

	code, body, err := d.do(runCtx, http.MethodPost, "/containers/"+d.containerName(h)+"/exec",
		map[string]any{
			"AttachStdout": true,
			"AttachStderr": true,
			// Stdin fechado: exec desta porta é comando, não sessão. E Tty
			// falso é o que MANTÉM stdout e stderr separados no stream
			// (garantia 16) — com tty os dois se fundem.
			"AttachStdin": false,
			"Tty":         false,
			"Cmd":         req.Command,
		})
	if err != nil {
		return nil, err
	}
	if code == http.StatusConflict {
		return nil, errs.Precondition(
			"o sandbox %s não está em execução; retome-o antes de rodar comandos", h.ID)
	}
	if code >= 300 {
		return nil, fail(code, body, "preparação do comando no sandbox")
	}
	var criado struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(body, &criado); err != nil || criado.ID == "" {
		return nil, errs.Wrap(errs.KindInternal, err, "resposta ilegível do Docker ao criar o exec")
	}

	inicio, err := json.Marshal(map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "pedido ilegível para o Docker")
	}
	httpReq, err := http.NewRequestWithContext(runCtx, http.MethodPost,
		"http://docker/"+d.apiVer+"/exec/"+criado.ID+"/start", strings.NewReader(string(inicio)))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// `stream` e não `client`: o prazo desta chamada é o do comando, e o
	// Timeout do cliente comum (30s) cortaria todo comando mais longo que isso
	// sem nada que explicasse.
	resp, err := d.stream.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errs.Wrap(errs.KindUnavailable, ctx.Err(), "execução interrompida pelo chamador")
		}
		if runCtx.Err() != nil {
			// Prazo estourado antes de qualquer byte: ainda é RESULTADO.
			return d.execResultado(ctx, criado.ID, "", "", false, true)
		}
		return nil, errs.Wrap(errs.KindUnavailable, err, "falha ao iniciar o comando no sandbox")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusConflict {
			return nil, errs.Precondition(
				"o sandbox %s não está em execução; retome-o antes de rodar comandos", h.ID)
		}
		return nil, fail(resp.StatusCode, out, "execução do comando no sandbox")
	}

	saida, erroPadrao := &bufferComTeto{max: teto}, &bufferComTeto{max: teto}
	lerErr := demuxBruto(resp.Body, saida, erroPadrao)

	if ctx.Err() != nil {
		return nil, errs.Wrap(errs.KindUnavailable, ctx.Err(), "execução interrompida pelo chamador")
	}
	expirou := runCtx.Err() != nil
	if lerErr != nil && !expirou {
		return nil, errs.Wrap(errs.KindUnavailable, lerErr, "fluxo do comando interrompido")
	}
	return d.execResultado(ctx, criado.ID,
		saida.String(), erroPadrao.String(), saida.cortou || erroPadrao.cortou, expirou)
}

// execResultado consulta o código de saída e monta o resultado.
//
// O contexto vem SEM o prazo do comando de propósito: quando o comando estourou
// o prazo, o contexto dele já está morto, e usá-lo aqui perderia justamente a
// informação de que o processo continua rodando lá dentro.
func (d *Docker) execResultado(ctx context.Context, execID, saida, erro string,
	cortou, expirou bool) (*ports.ExecResult, error) {

	res := &ports.ExecResult{
		ExitCode: -1, Stdout: saida, Stderr: erro, Truncated: cortou, TimedOut: expirou,
	}
	insp, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	code, body, err := d.do(insp, http.MethodGet, "/exec/"+execID+"/json", nil)
	if err != nil || code >= 300 {
		// Sem o código de saída, -1 é a resposta honesta: zero afirmaria
		// sucesso, e afirmar sucesso sem saber é a pior das três saídas.
		return res, nil
	}
	var out struct {
		Running  bool `json:"Running"`
		ExitCode *int `json:"ExitCode"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return res, nil
	}
	if !out.Running && out.ExitCode != nil {
		res.ExitCode = *out.ExitCode
	}
	if out.Running {
		// O processo ficou de pé: é prazo estourado, mesmo que o stream tenha
		// acabado antes. Dizer o contrário daria a um comando pendurado a cara
		// de um comando que terminou sem saída.
		res.TimedOut = true
	}
	return res, nil
}

// demuxBruto desmonta os quadros do Docker direto para dois buffers.
//
// Separado de `demux` porque as duas leituras querem coisas diferentes: o Tail
// quer LINHAS carimbadas, o exec quer os BYTES exatos de cada fluxo. Reaproveitar
// o de linhas aqui reconstruiria a saída com quebras que o comando não emitiu.
func demuxBruto(r io.Reader, saida, erro *bufferComTeto) error {
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		size := binary.BigEndian.Uint32(header[4:8])
		if size == 0 {
			continue
		}
		alvo := saida
		if header[0] == 2 {
			alvo = erro
		}
		// Lê SEMPRE o quadro inteiro, mesmo depois de bater o teto: parar de
		// ler deixaria o daemon escrevendo num cano cheio e o processo lá
		// dentro travado. O teto corta o que é GUARDADO, não o que é lido.
		if _, err := io.CopyN(alvo, r, int64(size)); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
	}
}

// ── logs ─────────────────────────────────────────────────────────────────────

// Tail segue o log do sandbox e morre junto com o chamador.
//
// O stream do Docker é MULTIPLEXADO quando não há tty: cada quadro traz um
// cabeçalho de 8 bytes com o fluxo e o tamanho. Ler isso como texto puro
// entregaria o cabeçalho binário grudado na primeira linha de cada quadro — um
// bug que passa despercebido até alguém procurar por um prefixo exato no log.
func (d *Docker) Tail(ctx context.Context, h ports.SandboxHandle, q ports.LogQuery, emit func(ports.LogLine) error) error {
	if q.Service != "" && q.Service != "sandbox" {
		// Um sandbox do Docker é UM processo; o compose interno da demanda roda
		// dentro dele e não é visível daqui. Nome desconhecido é NotFound, a
		// mesma resposta que o k8s dá para contêiner que não existe no pod.
		return errs.NotFound("processo %q no sandbox %s", q.Service, h.ID)
	}
	if _, err := d.Describe(ctx, h); err != nil {
		return err
	}

	path := fmt.Sprintf("/containers/%s/logs?stdout=1&stderr=1&timestamps=1&follow=%d",
		d.containerName(h), boolToInt(q.Follow))
	if q.TailLines > 0 {
		path += fmt.Sprintf("&tail=%d", q.TailLines)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/"+d.apiVer+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.stream.Do(req)
	if err != nil {
		if ctxEnded(ctx) {
			return nil
		}
		return errs.Wrap(errs.KindUnavailable, err, "falha ao seguir os logs")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		out, _ := io.ReadAll(resp.Body)
		return fail(resp.StatusCode, out, "leitura dos logs")
	}

	return demux(ctx, resp.Body, "sandbox", emit)
}

// demux desmonta os quadros do Docker e entrega linha a linha.
func demux(ctx context.Context, r io.Reader, service string, emit func(ports.LogLine) error) error {
	header := make([]byte, 8)
	for {
		if ctxEnded(ctx) {
			return nil
		}
		if _, err := io.ReadFull(r, header); err != nil {
			return streamEnd(ctx, err)
		}
		stream := "stdout"
		if header[0] == 2 {
			stream = "stderr"
		}
		size := binary.BigEndian.Uint32(header[4:8])
		if size == 0 {
			continue
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return streamEnd(ctx, err)
		}
		for _, raw := range strings.Split(strings.TrimRight(string(payload), "\n"), "\n") {
			at, text := splitTimestamp(raw)
			if err := emit(ports.LogLine{Service: service, Stream: stream, Text: text, At: at}); err != nil {
				return err // erro do emit sobe: é como se sabe que o cliente sumiu
			}
		}
	}
}

// streamEnd distingue "acabou" de "quebrou". Fim de stream e cancelamento do
// cliente são encerramento normal; o resto é falha de verdade.
func streamEnd(ctx context.Context, err error) error {
	if err == nil || ctxEnded(ctx) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return errs.Wrap(errs.KindUnavailable, err, "fluxo de logs interrompido")
}

func ctxEnded(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ═════════════════════════════════════════════════════════════════════════════
// Comum aos DOIS adaptadores deste pacote.
//
// Vive aqui, e não num terceiro arquivo, porque é pouco e porque duplicá-lo
// entre k8s.go e docker.go seria abrir a porta para os dois divergirem
// justamente nas convenções que precisam ser idênticas: as labels que
// identificam de quem é o sandbox, e a validação do que a porta exige.
// ═════════════════════════════════════════════════════════════════════════════

// Identificação hierárquica é LABEL, não nome (spec do substrato §1): o nome
// carrega só o que precisa ser curto e único; conta, demanda e tier ficam
// consultáveis sem parsear string.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelAccount   = "dop.dev/account"
	labelDemand    = "dop.dev/demand"
	labelSandbox   = "dop.dev/sandbox"
	labelTier      = "dop.dev/tier"
)

func labelsFor(spec ports.SandboxSpec) map[string]string {
	return map[string]string{
		labelManagedBy: "dop-core",
		labelAccount:   labelValue(spec.AccountID),
		labelDemand:    labelValue(spec.DemandID),
		labelSandbox:   labelValue(spec.ID),
		labelTier:      string(spec.Tier),
	}
}

// labelValue reduz um id ao alfabeto que k8s aceita em label (63 caracteres,
// alfanumérico com - _ . no meio). O Docker aceitaria qualquer coisa; usar a
// regra mais estrita nos dois é o que mantém a mesma consulta funcionando dos
// dois lados.
func labelValue(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-.")
	}
	return out
}

// validateSpec recusa o que a porta não admite. O launcher NUNCA inventa
// identidade nem nível de isolamento: id, namespace, imagem e tier vêm do
// domínio ou a chamada é inválida.
func validateSpec(spec ports.SandboxSpec) error {
	if strings.TrimSpace(spec.ID) == "" || strings.TrimSpace(spec.Namespace) == "" {
		return errs.Invalid("sandbox sem identificação: id e namespace são obrigatórios")
	}
	if strings.TrimSpace(spec.Image) == "" {
		return errs.Invalid("sandbox sem imagem")
	}
	if !ports.ValidIsolationTier(spec.Tier) {
		return errs.Invalid(
			"nível de isolamento não declarado — o substrato não escolhe por você")
	}
	return nil
}

// endpointsFromPorts converte portas publicadas em endpoints.
//
// Sem porta publicada, lista VAZIA — nunca um endpoint inventado. É garantia da
// porta: um adaptador que fabrique endpoint faz o cockpit oferecer link que
// não abre.
func endpointsFromPorts(exposed map[string]any, running bool) []ports.SandboxEndpoint {
	if len(exposed) == 0 {
		return nil
	}
	state := "stopped"
	if running {
		state = "running"
	}
	keys := make([]string, 0, len(exposed))
	for k := range exposed {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]ports.SandboxEndpoint, 0, len(keys))
	for _, k := range keys {
		num := k
		if i := strings.IndexByte(k, '/'); i > 0 {
			num = k[:i]
		}
		var p int32
		if _, err := fmt.Sscanf(num, "%d", &p); err != nil || p <= 0 {
			continue
		}
		// O Docker não nomeia porta. O nome vem do número — e é por isso que a
		// suíte de contrato NÃO promete nome de endpoint: no k8s ele vem do
		// `name` da porta do contêiner, aqui não existe de onde tirá-lo.
		out = append(out, ports.SandboxEndpoint{Name: fmt.Sprintf("port-%d", p), Port: p, State: state})
	}
	return out
}

// execLimites resolve prazo e teto de saída a partir da requisição.
//
// Os defaults são da PORTA e não de cada adaptador: um default por adaptador
// faria o mesmo comando ter prazos diferentes conforme onde o sandbox subiu, e
// a suíte de contrato — que mede os dois com a mesma régua — não teria como
// afirmar nada sobre nenhum dos dois.
func execLimites(req ports.ExecRequest) (time.Duration, int) {
	prazo := time.Duration(req.TimeoutSeconds) * time.Second
	if req.TimeoutSeconds <= 0 {
		prazo = ports.DefaultExecTimeout
	}
	teto := req.MaxOutputBytes
	if teto <= 0 {
		teto = ports.DefaultExecMaxOutputBytes
	}
	return prazo, teto
}

// bufferComTeto acumula até `max` bytes e ANOTA que cortou.
//
// Ele nunca devolve erro em Write: quem escreve nele é um laço de leitura de
// stream, e interromper a leitura por causa do teto deixaria o processo do outro
// lado travado num cano cheio. O teto limita o que é GUARDADO — a leitura segue
// até o fim, e é isso que permite colher o código de saída depois.
type bufferComTeto struct {
	max    int
	buf    []byte
	cortou bool
}

func (b *bufferComTeto) Write(p []byte) (int, error) {
	if espaco := b.max - len(b.buf); espaco > 0 {
		if len(p) <= espaco {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:espaco]...)
			b.cortou = true
		}
	} else if len(p) > 0 {
		b.cortou = true
	}
	return len(p), nil
}

func (b *bufferComTeto) String() string { return string(b.buf) }

// splitTimestamp separa o carimbo RFC3339 que os dois substratos prefixam
// quando se pede timestamps. Linha sem carimbo devolve instante zero — e é o
// DOMÍNIO que decide o que fazer com isso, não o adaptador chutando time.Now().
func splitTimestamp(raw string) (time.Time, string) {
	sp := strings.IndexByte(raw, ' ')
	if sp <= 0 {
		return time.Time{}, raw
	}
	at, err := time.Parse(time.RFC3339Nano, raw[:sp])
	if err != nil {
		return time.Time{}, raw
	}
	return at.UTC(), raw[sp+1:]
}
