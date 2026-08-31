// Adaptador de SecretStore sobre o Google Cloud Secret Manager.
//
// É o segundo adaptador REAL da porta (ADR-0001): serve a plataforma quando ela
// roda em GCP, enquanto o irmão k8s serve o cluster self-hosted. O MESMO
// conjunto de testes de contrato roda contra os dois — é isso, e não a
// intenção, que torna a troca possível.
//
// Como o k8s, fala com o serviço pela API, e usa a biblioteca OFICIAL: um
// cliente escrito à mão acertaria os casos felizes e erraria exatamente os que
// importam (código de erro, retry, checksum, resolução de alias).
//
// ─────────────────── MAPEAMENTO PORTA → SECRET MANAGER ───────────────────
//
// A porta tem Put/Get/Delete/Exists sobre um SecretRef PLANO. O Secret Manager
// tem duas camadas: o SEGREDO (contêiner, com política de réplica e IAM) e as
// VERSÕES (o material, imutável, numerado). Versionamento está FORA da porta de
// propósito (ver ports.SecretStore) — então o adaptador precisa esconder a
// segunda camada por completo. A decisão:
//
//	Put    → CreateSecret (ignorando AlreadyExists)
//	         + AddSecretVersion
//	         + confirmação POR NÚMERO (forte)
//	         + espera do alias `latest` (eventual)
//	         + destruição das versões anteriores.
//	Get    → AccessSecretVersion em ".../versions/latest".
//	Delete → DeleteSecret (leva o contêiner e TODAS as versões).
//	Exists → Get != nil.
//
// Por que Get lê `latest` e não uma versão nomeada: a porta não tem onde
// guardar número de versão — SecretRef é plano e o domínio não conhece versão.
// `latest` é o único endereço estável derivável só da referência. O preço disso
// está na seção de consistência, mais abaixo, e é a descoberta mais importante
// deste arquivo.
//
// Por que Put destrói as versões anteriores. A garantia 4 diz que Put
// SUBSTITUI. No adaptador k8s isso é literal: o valor antigo deixa de existir.
// Se aqui as versões antigas ficassem, a mesma referência continuaria
// resolvendo o segredo ANTIGO por número de versão — e "rotacionei a credencial
// vazada" passaria a significar coisas diferentes em cada adaptador. Uma porta
// cujo significado depende de quem a implementa não é porta.
//
// Por que Delete apaga o SEGREDO e não só a versão. A porta manda Delete ser
// idempotente e a ausência ser estado normal. Destruir só a versão deixaria
// para trás um contêiner vazio com IAM próprio — estado invisível pela porta,
// que ninguém limpa e que reaparece como "existe mas não tem valor".
//
// Isolamento entre contas (garantia 5) sai do NOME, como no k8s: ver secretID.
//
// ───────── CONSISTÊNCIA: A GARANTIA 1 NÃO É CUMPRÍVEL NO GCP REAL ─────────
//
// A porta promete leitura-após-escrita IMEDIATA. O Google documenta o oposto,
// em https://cloud.google.com/secret-manager/docs/reference/consistency :
//
//   - "adding a secret version and then immediately accessing that secret
//     version BY VERSION NUMBER is a strongly consistent operation";
//   - "This doesn't apply when you access a secret version using aliases or
//     latest";
//   - "Other operations within Secret Manager are eventually consistent",
//     e convergem "typically within minutes, but may take a few hours".
//
// Ou seja: o único caminho fortemente consistente é o que a porta NÃO pode
// usar, porque exige carregar o número da versão — e versão é justamente o que
// o SecretRef não tem. Um Get logo depois de um Put pode, legitimamente,
// devolver (nil, nil) no GCP real, que pela porta significa "não existe": a
// credencial recém-gravada apareceria como ausente.
//
// Isto NÃO é defeito de implementação e não se conserta dentro deste arquivo.
// É decisão de arquitetura pendente — ou a porta passa a devolver um
// identificador de versão no Put para o domínio guardar (e Get lê por número,
// forte), ou a garantia 1 vira "leitura-após-escrita dentro do processo que
// escreveu" e o domínio precisa tolerar ausência transitória.
//
// O que este adaptador faz enquanto isso, e por quê:
//
//  1. confirma a gravação POR NÚMERO (forte, sempre funciona) — prova que o
//     material foi aceito e voltou byte a byte;
//  2. ESPERA o alias `latest` alcançar a versão nova, com teto configurável.
//     O Put não retorna antes disso. Se não convergir no teto, devolve
//     KindUnavailable dizendo exatamente isso.
//
// O passo 2 é caro e pode falhar no GCP real. É deliberado: um Put lento e um
// erro explícito são preferíveis a um Get silencioso devolvendo "não existe"
// para uma credencial que acabou de ser gravada. No emulador ele termina na
// primeira tentativa — e é por isso que ele não pode ser confundido com prova
// de que a garantia vale em produção.
//
// ─────────────── ONDE O EMULADOR LOCAL É MAIS PERMISSIVO ───────────────
//
// O ambiente local usa um emulador da COMUNIDADE (o Google não publica nenhum;
// ver P-17 no ROADMAP). Ele é mais frouxo que o GCP em pontos que fariam a
// suíte passar aqui e QUEBRAR em produção. Cada divergência abaixo foi
// verificada contra o emulador rodando, e cada uma tem uma defesa NESTE
// arquivo — a defesa é o que impede o ambiente local de esconder o caminho de
// produção:
//
//  1. RÉPLICA. O emulador aceita CreateSecret SEM o campo `replication` e
//     inventa "automatic". A referência REST do Google marca o campo como
//     "Required" (o .proto, mais novo, diz "Optional" por causa dos segredos
//     regionais — os dois discordam entre si). Defesa: mandamos
//     Replication_Automatic SEMPRE, explicitamente. Nunca dependemos do
//     default de ninguém, muito menos de um default sobre o qual a própria
//     documentação do fornecedor está dividida.
//
//  2. FORMATO DO NOME. O emulador aceita QUALQUER secretId — ponto, espaço,
//     barra, maiúscula, 300 caracteres, tudo respondeu 200. O GCP real
//     documenta "maximum length of 255 characters ... letters, numerals, and
//     the hyphen (-) and underscore (_)". Defesa: validateSecretID roda ANTES
//     de cada chamada, e o nome que construímos já nasce dentro do alfabeto.
//
//  3. TAMANHO DO VALOR. O emulador guardou 128 KiB sem reclamar. O GCP real
//     documenta 64 KiB por versão. Defesa: maxPayloadBytes, checado antes de
//     sair da máquina.
//
//  4. CHECKSUM. O emulador devolve dataCrc32c = 0 SEMPRE (não calcula) e ignora
//     o checksum enviado. O GCP real verifica na escrita e SEMPRE devolve o
//     valor na leitura — gera um se o cliente não mandou. Defesa: enviamos o
//     CRC (o real valida) e, na leitura, só verificamos quando ele vem
//     diferente de zero. Ficaria uma assimetria — integridade conferida só em
//     produção — se não fosse a confirmação do Put, que compara os BYTES lidos
//     com os gravados e vale nos dois ambientes.
//
//  5. ALIAS "latest". No emulador, `latest` CAI PARA TRÁS: com as versões 3
//     (desabilitada) e 2 (destruída), ele serve a versão 1, e a mensagem de
//     ausência é "No enabled versions found". No GCP real `latest` é "an alias
//     to the most recently CREATED SecretVersion", sem olhar o estado: se ela
//     estiver desabilitada ou destruída, o acesso falha. Defesa possível apenas
//     parcial: pelo desenho acima, a versão de maior número deste adaptador
//     está SEMPRE habilitada (Put destrói as anteriores, Delete leva tudo),
//     então os dois ambientes coincidem. A divergência aparece se alguém
//     desabilitar uma versão POR FORA — console, Terraform, resposta a
//     incidente. Aí, e só aí, o local devolve o valor ANTIGO e a produção
//     devolve ausência. É o ponto em que a suíte passa aqui e pode falhar lá.
//
//  6. PROPAGAÇÃO. O emulador é uma tabela em memória e anuncia "0ms". O GCP
//     real é eventualmente consistente para tudo que não seja acesso por
//     número — ver a seção de consistência acima. NENHUM teste local exercita
//     esse atraso, e é dele que sai a única garantia da porta que não se
//     sustenta em produção.
//
//  7. COTAS. O emulador não tem nenhuma. O GCP real publica, entre outras:
//     AddSecretVersion a 2 qps / 120 qpm POR SEGREDO; Destroy/Disable/Enable a
//     1 qps / 60 qpm POR VERSÃO; e, por projeto, 90.000 acessos/min mas apenas
//     600 leituras/min e 600 ESCRITAS/min. Um Put deste adaptador gasta de 3 a
//     5 dessas operações (create + addVersion + access + list + destroys), o
//     que coloca o teto prático em torno de 150 gravações por minuto no
//     projeto inteiro. E a própria suíte de contrato grava a MESMA referência
//     várias vezes em sequência, o que no GCP real esbarra no limite por
//     segredo. Defesa: erros de cota viram KindUnavailable (retryável) — mas
//     nada aqui simula a cota, e nenhum teste local vai encontrá-la.
//
//  8. IAM. O emulador não tem controle de acesso NENHUM: qualquer chamador lê
//     qualquer segredo. Em produção o isolamento por nome é a PRIMEIRA barreira
//     e a política de IAM da conta de serviço é a segunda — e é a segunda que
//     nenhum teste local exercita. O que a suíte prova localmente sobre
//     isolamento (garantia 5) é só a metade que vive no nome.
//
//  9. REUSO DE NOME APÓS DELETE. No emulador, apagar e recriar com o mesmo
//     nome funciona no ato. No GCP real DeleteSecret é irreversível e imediato,
//     mas os METADADOS são eventualmente consistentes: recriar em seguida pode
//     bater em AlreadyExists ou criar um segredo que ainda não aparece. A suíte
//     de contrato faz exatamente esse ciclo.
//
//  10. DURABILIDADE. O emulador é memória pura: o /data que a imagem declara
//     fica vazio, não há flag de import/export e um restart apaga TUDO —
//     verificado. Reiniciar o pod some com toda credencial do ambiente local.
//     No GCP real o segredo é durável e replicado. Nenhuma defesa é possível
//     aqui; a consequência está em dop-infra/docs/ambiente-local.md.
package secretstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"regexp"
	"strconv"
	"strings"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// maxPayloadBytes é o teto documentado do GCP real (64 KiB por versão).
// Checado AQUI porque o emulador aceita mais — divergência 3.
const maxPayloadBytes = 64 * 1024

// defaultPropagation é quanto o Put espera o alias `latest` alcançar a versão
// recém-gravada. No emulador a primeira tentativa já basta. No GCP real este é
// o teto da tentativa de honrar a garantia 1 — ver a seção de consistência.
const defaultPropagation = 30 * time.Second

type GCP struct {
	client      *secretmanager.Client
	parent      string // projects/<id>
	propagation time.Duration
}

type GCPConfig struct {
	// ProjectID é o projeto que HOSPEDA os segredos. Obrigatório: sem ele os
	// nomes sairiam como "projects//secrets/..." e a falha apareceria longe da
	// causa, na primeira credencial gravada.
	ProjectID string
	// Endpoint aponta para o EMULADOR ("host:porta"). Vazio = GCP real, com
	// credencial padrão do ambiente (ADC). Preenchido = sem autenticação e sem
	// TLS, que é o que o emulador oferece — e é por isso que preenchê-lo em
	// produção seria mandar segredo em texto claro para um endereço arbitrário.
	//
	// Não existe variável oficial de emulador para o Secret Manager (o Google
	// tem STORAGE_EMULATOR_HOST, PUBSUB_EMULATOR_HOST e afins, mas nenhuma
	// aqui — a biblioteca oficial não lê nenhuma). Esta é NOSSA.
	Endpoint string
	// Propagation é o teto da espera de leitura-após-escrita. Zero = padrão.
	Propagation time.Duration
}

// NewGCP abre o cliente. Devolve erro porque montar o cliente resolve
// credencial (ADC) e pode falhar — e falhar no BOOT é melhor que falhar na
// primeira credencial gravada, com o processo já se dizendo saudável.
func NewGCP(ctx context.Context, cfg GCPConfig) (*GCP, error) {
	if strings.TrimSpace(cfg.ProjectID) == "" {
		return nil, errs.Invalid("SECRET_PROJECT é obrigatória quando SECRET_BACKEND=gcp")
	}
	var opts []option.ClientOption
	if cfg.Endpoint != "" {
		// As três opções andam JUNTAS. Só WithEndpoint faria a biblioteca
		// continuar procurando ADC e exigindo TLS, e o erro apareceria como
		// "transport: authentication handshake failed" — que não diz nada
		// sobre a causa real.
		opts = append(opts,
			option.WithEndpoint(cfg.Endpoint),
			option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		)
	}
	c, err := secretmanager.NewClient(ctx, opts...)
	if err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, err, "falha ao abrir o cliente do Secret Manager")
	}
	prop := cfg.Propagation
	if prop <= 0 {
		prop = defaultPropagation
	}
	return &GCP{client: c, parent: "projects/" + cfg.ProjectID, propagation: prop}, nil
}

// Close libera a conexão gRPC. A porta não tem Close (o k8s e o em memória não
// precisam de um); quem fecha é o composition root, junto do resto.
func (g *GCP) Close() error { return g.client.Close() }

// ───────────────────────── nome e isolamento ─────────────────────────

// gcpSecretID é o alfabeto que o GCP real aceita. O emulador aceita qualquer
// coisa (divergência 2), então esta expressão é a única coisa entre um nome
// inválido e uma falha que só apareceria em produção.
var gcpSecretID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

// compMax limita cada pedaço legível do nome para o total caber em 255.
const compMax = 60

// secretID mapeia a referência lógica para o nome do segredo. O isolamento
// entre contas está AQUI — referência da conta A jamais resolve segredo da
// conta B (garantia 5).
//
// Duas diferenças deliberadas em relação ao irmão k8s:
//
//   - o separador é "_", que sanitize NUNCA produz. No k8s o separador é "-",
//     que sanitize produz o tempo todo, e por isso lá os nomes são AMBÍGUOS:
//     conta "a-b" com tipo "c" e conta "a" com tipo "b-c" geram o MESMO nome.
//     É um vazamento entre contas latente, relatado à parte;
//
//   - o nome termina com uma impressão digital da tupla CRUA. sanitize é
//     lossy ("a.b" e "a-b" viram a mesma coisa), então a parte legível sozinha
//     não basta. A impressão digital é o que torna a garantia 5 uma
//     propriedade do CÓDIGO, e não do formato que os identificadores por acaso
//     têm hoje.
func (g *GCP) secretID(ref ports.SecretRef) string {
	return fmt.Sprintf("dop_%s_%s_%s_%s",
		clamp(sanitize(ref.AccountID)),
		clamp(sanitize(ref.Kind)),
		clamp(sanitize(ref.OwnerID)),
		fingerprint(ref))
}

func clamp(s string) string {
	if len(s) > compMax {
		return s[:compMax]
	}
	return s
}

// fingerprint distingue tuplas que sanitize confundiria. O COMPRIMENTO de cada
// campo entra no hash: sem ele, {"ab",""} e {"a","b"} teriam a mesma digestão.
func fingerprint(r ports.SecretRef) string {
	h := sha256.New()
	for _, s := range []string{r.AccountID, r.Kind, r.OwnerID} {
		fmt.Fprintf(h, "%d:", len(s))
		_, _ = h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// validateSecretID nunca cita o valor do segredo, só a forma do nome.
func validateSecretID(id string) error {
	if !gcpSecretID.MatchString(id) {
		return errs.Invalid(
			"nome de segredo fora do formato aceito pelo Secret Manager (%d caracteres)", len(id))
	}
	return nil
}

func (g *GCP) secretName(id string) string { return g.parent + "/secrets/" + id }

// ───────────────────────── operações da porta ─────────────────────────

var crcTable = crc32.MakeTable(crc32.Castagnoli)

func (g *GCP) Put(ctx context.Context, ref ports.SecretRef, v ports.SecretValue) error {
	if len(v) > maxPayloadBytes {
		return errs.Invalid("segredo maior que o limite do Secret Manager (%d bytes; máximo %d)",
			len(v), maxPayloadBytes)
	}
	id := g.secretID(ref)
	if err := validateSecretID(id); err != nil {
		return err
	}

	// 1. o contêiner. AlreadyExists é o caminho NORMAL do segundo Put — não é
	//    erro, é o segredo já existir. Replication vai explícito (divergência 1).
	_, err := g.client.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   g.parent,
		SecretId: id,
		Secret: &secretmanagerpb.Secret{
			Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{
					Automatic: &secretmanagerpb.Replication_Automatic{},
				},
			},
			Labels: map[string]string{"managed-by": "dop-core"},
		},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return wrapGCP(err, "falha ao criar o segredo no Secret Manager")
	}

	// 2. o material. O CRC é conferido pelo GCP real na escrita; o emulador o
	//    ignora (divergência 4).
	crc := int64(crc32.Checksum(v, crcTable))
	ver, err := g.client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  g.secretName(id),
		Payload: &secretmanagerpb.SecretPayload{Data: v, DataCrc32C: &crc},
	})
	if err != nil {
		return wrapGCP(err, "falha ao gravar a versão do segredo")
	}
	n, err := versionNumber(ver.GetName())
	if err != nil {
		return err
	}

	// 3. confirmação FORTE, por número: é o único acesso que o Google promete
	//    ser imediato. Prova que o material foi aceito e volta idêntico.
	if err := g.confirmByNumber(ctx, ver.GetName(), v); err != nil {
		return err
	}

	// 4. confirmação EVENTUAL, pelo caminho que o Get usa. Ver a seção de
	//    consistência: é aqui que a garantia 1 é honrada — ou falha alto.
	if err := g.awaitLatest(ctx, id, n); err != nil {
		return err
	}

	// 5. "Put SUBSTITUI" (garantia 4): o valor anterior deixa de ser legível.
	return g.destroyOlder(ctx, id, n)
}

// confirmByNumber lê a versão recém-criada pelo nome COMPLETO e compara os
// bytes. É a integridade ponta a ponta que não depende do checksum — que o
// emulador não calcula (divergência 4).
func (g *GCP) confirmByNumber(ctx context.Context, name string, v ports.SecretValue) error {
	resp, err := g.client.AccessSecretVersion(ctx,
		&secretmanagerpb.AccessSecretVersionRequest{Name: name})
	if err != nil {
		return wrapGCP(err, "falha ao confirmar a versão recém-gravada do segredo")
	}
	if !bytes.Equal(resp.GetPayload().GetData(), v) {
		// Sem citar nenhum dos dois valores (garantia 6).
		return errs.Internal("o Secret Manager devolveu conteúdo diferente do que foi gravado")
	}
	return nil
}

// awaitLatest espera o alias `latest` alcançar a versão gravada.
//
// Sem isto, "grava e volta" seria leitura-após-escrita apenas no emulador: no
// GCP real o alias é eventualmente consistente, e um Get logo depois do Put
// devolveria (nil, nil) — que pela porta significa "não existe". Uma
// credencial recém-gravada apareceria como ausente, em silêncio.
func (g *GCP) awaitLatest(ctx context.Context, id string, want int64) error {
	deadline := time.Now().Add(g.propagation)
	wait := 25 * time.Millisecond
	for {
		resp, err := g.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
			Name: g.secretName(id) + "/versions/latest",
		})
		switch {
		case err == nil:
			got, verr := versionNumber(resp.GetName())
			if verr != nil {
				return verr
			}
			// >= e não ==: outro Put concorrente pode já ter passado na
			// frente, e nesse caso a propagação alcançou de sobra.
			if got >= want {
				return nil
			}
		case status.Code(err) == codes.NotFound, status.Code(err) == codes.FailedPrecondition:
			// ainda não propagou — é exatamente o caso que esta espera cobre
		default:
			return wrapGCP(err, "falha ao confirmar a visibilidade do segredo")
		}

		if !time.Now().Before(deadline) {
			return errs.New(errs.KindUnavailable,
				"o Secret Manager não tornou a versão %d visível por `latest` em %s: "+
					"a gravação foi aceita, mas a leitura-após-escrita não se confirmou",
				want, g.propagation)
		}
		select {
		case <-ctx.Done():
			return errs.Wrap(errs.KindUnavailable, ctx.Err(),
				"contexto encerrado antes de confirmar a visibilidade do segredo")
		case <-time.After(wait):
		}
		if wait < time.Second {
			wait *= 2
		}
	}
}

// destroyOlder apaga o material das versões anteriores. Roda DEPOIS das
// confirmações: primeiro o valor novo está de pé, só então o antigo cai.
//
// O erro SOBE em vez de ser engolido. Se a destruição falhar, o valor antigo
// continua legível por número de versão e a garantia 4 não foi cumprida por
// inteiro — silenciar isso seria a plataforma achar que rotacionou uma
// credencial que continua valendo. A mensagem diz que o valor novo já está
// ativo, para o operador saber que repetir o Put é seguro.
func (g *GCP) destroyOlder(ctx context.Context, id string, keep int64) error {
	it := g.client.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{
		Parent: g.secretName(id),
	})
	var stale []string
	for {
		v, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return wrapGCP(err, "o valor novo do segredo já está ativo, mas não foi "+
				"possível listar as versões anteriores para destruí-las")
		}
		if v.GetState() == secretmanagerpb.SecretVersion_DESTROYED {
			continue
		}
		n, err := versionNumber(v.GetName())
		if err != nil {
			return err
		}
		if n < keep {
			stale = append(stale, v.GetName())
		}
	}
	for _, name := range stale {
		_, err := g.client.DestroySecretVersion(ctx,
			&secretmanagerpb.DestroySecretVersionRequest{Name: name})
		// FailedPrecondition/NotFound = já destruída, provavelmente por uma
		// corrida com outro Put. O estado desejado é esse mesmo.
		if err != nil && status.Code(err) != codes.FailedPrecondition &&
			status.Code(err) != codes.NotFound {
			return wrapGCP(err, "o valor novo do segredo já está ativo, mas o valor "+
				"anterior NÃO foi destruído e continua legível")
		}
	}
	return nil
}

func (g *GCP) Get(ctx context.Context, ref ports.SecretRef) (ports.SecretValue, error) {
	id := g.secretID(ref)
	if err := validateSecretID(id); err != nil {
		return nil, err
	}
	resp, err := g.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: g.secretName(id) + "/versions/latest",
	})
	switch {
	case err == nil:
	case status.Code(err) == codes.NotFound:
		// garantia 2: ausente devolve nil, não erro. Cobre tanto "o segredo não
		// existe" quanto "o segredo existe e não tem versão utilizável".
		return nil, nil
	case status.Code(err) == codes.FailedPrecondition:
		// `latest` existe mas está desabilitada ou destruída (divergência 5).
		// Pela porta não há valor, e isso é o mesmo que ausência: traduzir para
		// erro faria o chamador tratar "credencial ainda não configurada" como
		// falha do sistema.
		return nil, nil
	default:
		return nil, wrapGCP(err, "falha ao ler o segredo no Secret Manager")
	}

	data := resp.GetPayload().GetData()
	// Só verifica quando o servidor informa o checksum: o emulador devolve
	// sempre zero (divergência 4), e exigi-lo quebraria o ambiente local por um
	// defeito que não é do adaptador.
	if c := resp.GetPayload().DataCrc32C; c != nil && *c != 0 {
		if uint32(*c) != crc32.Checksum(data, crcTable) {
			return nil, errs.Internal("segredo corrompido em trânsito: checksum divergente")
		}
	}
	if data == nil {
		// Valor vazio é VALOR, não ausência: devolvemos fatia não-nula para que
		// Exists concorde com o k8s e com o adaptador em memória.
		return ports.SecretValue{}, nil
	}
	return ports.SecretValue(data), nil
}

func (g *GCP) Delete(ctx context.Context, ref ports.SecretRef) error {
	id := g.secretID(ref)
	if err := validateSecretID(id); err != nil {
		return err
	}
	err := g.client.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: g.secretName(id)})
	if err != nil && status.Code(err) != codes.NotFound { // garantia 3: idempotente
		return wrapGCP(err, "falha ao remover o segredo no Secret Manager")
	}
	return nil
}

func (g *GCP) Exists(ctx context.Context, ref ports.SecretRef) (bool, error) {
	v, err := g.Get(ctx, ref)
	return v != nil, err
}

// ───────────────────────── tradução de erro ─────────────────────────

// versionNumber extrai o N de ".../versions/N". O nome vem do servidor; se ele
// não tiver essa forma, alguma premissa deste adaptador deixou de valer — e é
// melhor dizer isso do que seguir com um número inventado.
func versionNumber(name string) (int64, error) {
	i := strings.LastIndex(name, "/versions/")
	if i < 0 {
		return 0, errs.Internal("nome de versão inesperado vindo do Secret Manager")
	}
	n, err := strconv.ParseInt(name[i+len("/versions/"):], 10, 64)
	if err != nil {
		return 0, errs.Internal("número de versão inesperado vindo do Secret Manager")
	}
	return n, nil
}

// wrapGCP traduz o código gRPC para o Kind do domínio.
//
// A mensagem original do servidor NÃO entra: ela carrega o nome completo do
// segredo, que revela conta e proprietário. Erro sobe para log (garantia 6).
func wrapGCP(err error, msg string) error {
	// O prazo estourado com a conexão caída NÃO chega como status do servidor:
	// a biblioteca devolve o erro do CONTEXTO, e status.Code() sobre ele
	// responde Unknown — que cairia no default e viraria KindInternal. Ou seja:
	// "Secret Manager fora do ar" seria reportado como defeito nosso, mandando
	// quem investiga procurar no lugar errado. É o mesmo engano que a porta
	// IdentityProvider proíbe por escrito na garantia 7.
	//
	// Encontrado rodando a suíte com o emulador INALCANÇÁVEL — o caminho que
	// deveria PULAR o teste, e que só é exercitado quando alguém o exercita de
	// propósito.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errs.Wrap(errs.KindUnavailable, err, "%s", msg)
	}
	var kind errs.Kind
	switch status.Code(err) {
	case codes.NotFound:
		kind = errs.KindNotFound
	case codes.AlreadyExists:
		kind = errs.KindAlreadyExists
	case codes.InvalidArgument:
		kind = errs.KindInvalid
	case codes.PermissionDenied:
		kind = errs.KindPermission
	case codes.Unauthenticated:
		kind = errs.KindUnauthorized
	case codes.FailedPrecondition:
		kind = errs.KindPrecondition
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.ResourceExhausted:
		// ResourceExhausted é COTA (divergência 7): temporário e retryável, não
		// defeito do chamador.
		kind = errs.KindUnavailable
	default:
		kind = errs.KindInternal
	}
	return errs.New(kind, "%s (código %s)", msg, status.Code(err))
}

var _ ports.SecretStore = (*GCP)(nil)
