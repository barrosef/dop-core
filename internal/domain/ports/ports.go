// Package ports declara as PORTAS de infraestrutura, em linguagem do domínio.
//
// Regra da ADR-0001: o domínio define a porta com a superfície mais estreita de
// que precisa; adaptadores de fornecedor ficam em internal/adapter e são
// escolhidos por configuração no composition root. Nenhum SDK cruza esta
// fronteira, e capacidade que não mapeia entre adaptadores fica FORA da porta.
package ports

import (
	"context"
	"time"
)

// ───────────────────────── SecretStore ─────────────────────────

// SecretRef é uma referência LÓGICA e opaca: só o adaptador sabe resolvê-la
// (caminho no Secret Manager, nome de Secret no k8s). O domínio nunca conhece
// caminho, namespace nem nome de segredo.
type SecretRef struct {
	AccountID string
	Kind      string // integration_credential
	OwnerID   string
}

// SecretValue é opaco por construção: sem String() útil, não serializa em log.
type SecretValue []byte

func (SecretValue) String() string { return "***" }

// SecretStore — quatro operações e nada além.
//
// Garantias verificadas pelo conjunto de testes de contrato, em TODO adaptador:
//  1. leitura-após-escrita: Put seguido de Get devolve o mesmo valor, imediatamente;
//  2. Get de referência inexistente devolve (nil, nil) — não erro;
//  3. Delete é idempotente;
//  4. Put sobre referência existente substitui;
//  5. isolamento: referência da conta A jamais resolve segredo da conta B;
//  6. o valor nunca aparece em log, erro ou stack trace.
//
// Versionamento fica FORA da porta: o Secret Manager tem versões, o Secret do
// k8s é plano. Capacidade que não mapeia não entra.
type SecretStore interface {
	Put(ctx context.Context, ref SecretRef, v SecretValue) error
	Get(ctx context.Context, ref SecretRef) (SecretValue, error)
	Delete(ctx context.Context, ref SecretRef) error
	Exists(ctx context.Context, ref SecretRef) (bool, error)
}

// ───────────────────────── ObjectStore ─────────────────────────

type ObjectRef struct {
	Bucket string
	Key    string
}

type ObjectMeta struct {
	Size        int64
	ContentType string
	UpdatedAt   time.Time
}

// ObjectStore guarda artefatos de conhecimento, uploads do chat e diagramas.
// SignedPutURL existe para o binário do upload NÃO passar pelo BFF.
//
// Garantias verificadas pela suíte de contrato, em TODO adaptador:
//  1. leitura-após-escrita: Put seguido de Get devolve os mesmos bytes, imediatamente;
//  2. Put SUBSTITUI o objeto inteiro — sem merge, sem versão — e a troca é
//     atômica para quem lê: nunca se lê metade de um objeto;
//  3. ausência é ERRO, com KindNotFound, em Get e Stat. Diferente do SecretStore,
//     que devolve (nil, nil): lá a ausência é estado normal do fluxo de
//     credencial; aqui o artefato pedido não existir é falha do caso de uso;
//  4. Delete é idempotente: remover o que não existe devolve nil;
//  5. a chave é OPACA e PLANA. Pode conter "/", mas isso NÃO cria hierarquia:
//     "a/b" e "a/b/c" são dois objetos independentes, "a/b" não é prefixo
//     navegável, e chave nenhuma escapa do bucket ("../x" é nome literal);
//  6. o bucket isola: a mesma chave em buckets diferentes são objetos diferentes;
//  7. Stat.Size é o tamanho exato do conteúdo gravado (zero é válido);
//     ContentType devolve o que foi gravado, e "application/octet-stream" quando
//     o Put não informou; UpdatedAt nunca é zero e não regride entre escritas;
//  8. SignedPutURL/SignedGetURL ou devolvem URL não vazia sem tocar no objeto,
//     ou falham com KindUnavailable — NUNCA string vazia com erro nil. A
//     capacidade não existe em todo backend (armazenamento em arquivo não tem o
//     que assinar) e a alternativa é passar o binário pelo BFF; o chamador
//     precisa poder DESCOBRIR isso em vez de receber uma URL que não funciona;
//  9. seguro para uso concorrente.
type ObjectStore interface {
	Put(ctx context.Context, ref ObjectRef, content []byte, contentType string) error
	Get(ctx context.Context, ref ObjectRef) ([]byte, error)
	Delete(ctx context.Context, ref ObjectRef) error
	Stat(ctx context.Context, ref ObjectRef) (*ObjectMeta, error)
	SignedPutURL(ctx context.Context, ref ObjectRef, ttl time.Duration) (string, error)
	SignedGetURL(ctx context.Context, ref ObjectRef, ttl time.Duration) (string, error)
}

// ───────────────────────── IdentityProvider ─────────────────────────

// Principal é o resultado NORMALIZADO da verificação de token. Claims de
// Firebase não cruzam esta fronteira — é o que permite trocar por Keycloak,
// Zitadel ou Ory sem tocar no domínio.
type Principal struct {
	// Subject é o ÚNICO campo obrigatório (garantia 3).
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	AvatarURL     string
	Providers     []string
}

// IdentityProvider responde UMA pergunta: quem está chamando?
//
// A porta tem uma operação só de propósito. Emitir, renovar, revogar e
// administrar usuário é trabalho do provedor de identidade, não do domínio; o
// que o domínio precisa é que a resposta "quem é" tenha a MESMA forma vindo do
// Firebase (GCP) ou de um Keycloak/Dex/Authentik dentro do cluster.
//
// Garantias verificadas pela suíte de contrato, em TODO adaptador:
//
//  1. token que não se prova é KindUnauthorized, SEMPRE e só ele: vazio,
//     malformado, sem as três partes, base64 ou JSON ilegível, algoritmo não
//     aceito ("none" e HS* inclusive — trocar o algoritmo é a forma clássica de
//     transformar chave pública em segredo compartilhado), assinatura inválida,
//     assinado por chave desconhecida, expirado, ainda não válido (nbf), de
//     outro emissor, de outra audiência, ou sem sujeito. Um Kind só, porque do
//     ponto de vista do domínio existe uma decisão só: esta chamada não está
//     autenticada;
//
//  2. a mensagem do erro NUNCA contém o token nem pedaço dele — nem prefixo,
//     nem sufixo, nem o payload decodificado, nem a claim crua. Erro sobe para
//     log, e token em log é credencial em repouso: quem lê o log entra como o
//     dono. A mensagem descreve a CAUSA em português para o operador
//     ("token expirado", "emissor inesperado"); o material do token fica fora;
//
//  3. com erro nil, Subject NUNCA é vazio. É o único campo obrigatório do
//     Principal, e é a identidade estável do sujeito DENTRO do emissor —
//     emissor diferente é espaço de identidades diferente, e é por isso que o
//     domínio guarda Subject junto do emissor que o produziu, nunca sozinho;
//
//  4. Email, Name e AvatarURL são OPCIONAIS: vazio quer dizer "o emissor não
//     informou", jamais erro. Login por telefone, por chave de acesso ou por
//     SSO corporativo sem escopo de perfil não tem e-mail nenhum para dar, e um
//     adaptador que exigisse e-mail tornaria a porta inutilizável nesses casos;
//
//  5. EmailVerified é FALSE quando o emissor não informa — não é erro. Ausência
//     de afirmação não é afirmação: se o default fosse true, um emissor calado
//     promoveria todo mundo a e-mail verificado, e a checagem que protege
//     vinculação de conta por e-mail viraria carimbo;
//
//  6. Providers é INFORMATIVO: como o sujeito se autenticou e/ou quais
//     identidades estão vinculadas, em nomes minúsculos, sem repetição e sem
//     entrada vazia ("password", "google.com", "oidc"). Adaptador que não sabe
//     dizer devolve lista VAZIA — nunca inventa, nunca devolve nome interno de
//     fornecedor, nunca devolve nil "significando alguma coisa". Como um
//     adaptador legítimo pode não saber, DECISÃO DE AUTORIZAÇÃO NÃO PODE
//     DEPENDER DESTE CAMPO;
//
//  7. VerifyToken PODE fazer I/O — descobrir o emissor e buscar a chave pública
//     é parte de verificar. Quando esse I/O falha (rede, emissor fora do ar,
//     resposta ilegível, contexto cancelado ou prazo esgotado) o erro é
//     KindUnavailable, NUNCA KindUnauthorized. Confundir os dois converte uma
//     indisponibilidade do emissor em "seu login é inválido" para TODOS os
//     usuários ao mesmo tempo, manda o usuário trocar senha que está certa e
//     manda a equipe caçar o defeito no lugar errado;
//
//  8. a chave pública é CACHEADA, e revalidada quando aparece um `kid`
//     desconhecido. As duas metades são obrigatórias: buscar a chave a cada
//     requisição é negação de serviço contra o próprio emissor, e não
//     revalidar transforma a rotação de chave — rotina no Keycloak e no
//     Firebase — em queda total de login. A revalidação é limitada no tempo:
//     um atacante que mande tokens com `kid` aleatório não pode virar um
//     gerador de tráfego contra o emissor;
//
//  9. verificar não tem efeito colateral, é determinístico e é seguro para uso
//     concorrente: o mesmo token, no mesmo instante, dá o mesmo resultado, e
//     nada no provedor muda por tê-lo verificado;
//
//  10. o token chega CRU, com ou sem o prefixo "Bearer " que a borda HTTP
//     carrega. Token vazio é KindUnauthorized decidido SEM I/O nenhum — quem
//     chama sem credencial não pode custar uma ida ao emissor;
//
//  11. claim crua não cruza a porta. O domínio enxerga este struct e nada mais:
//     nem mapa de claims, nem token original, nem bloco específico de
//     fornecedor. É esta garantia que faz a troca de emissor ser fiação.
//
// FORA da porta, de propósito:
//
//   - EMISSÃO, renovação e revogação de token, e todo o CRUD de usuário. É a
//     superfície mais assimétrica entre provedores e a que mais amarra: o
//     domínio nunca precisou dela para responder "quem está chamando";
//
//   - CHECAGEM DE REVOGAÇÃO por requisição. O Firebase oferece (checkRevoked,
//     com ida ao servidor a cada chamada); o OIDC genérico não oferece nada
//     equivalente sem introspecção, que nem todo emissor publica. Prometer o
//     que só um cumpre seria a abstração vazando — a expiração curta do token
//     é o que limita a janela nos dois;
//
//   - PAPÉIS, custom claims e escopos. Autorização é do domínio (conta,
//     hierarquia, papel), e trazer isso do emissor faria a política de acesso
//     morar no provedor de identidade de cada instalação;
//
//   - MULTI-TENANT do provedor, tolerância de relógio, TTL de cache e URL de
//     descoberta: são AJUSTE do adaptador, feitos no composition root por
//     variável de ambiente. Não são vocabulário do domínio.
type IdentityProvider interface {
	VerifyToken(ctx context.Context, raw string) (*Principal, error)
}

// ───────────────────────── EventBus ─────────────────────────

type Event struct {
	ID          string
	AccountID   string
	Aggregate   string
	AggregateID string
	Type        string
	Payload     []byte
	OccurredAt  time.Time
}

// Handler processa um evento. DEVE ser idempotente: a entrega é ao-menos-uma-vez
// (ADR-0019). Erro devolvido provoca retry com backoff; após o teto, DLQ.
type Handler func(ctx context.Context, e Event) error

// EventBus transporta BYTES, não structs.
//
// Payload é o envelope JSON do evento — o mesmo que o outbox grava. Essa frase
// é a garantia mais importante desta porta, porque a violação dela é silenciosa:
// um adaptador que reserialize ports.Event manda Payload []byte em base64, o
// outro lado não reconhece nada e TODA entrega é descartada sem erro nenhum.
//
// Garantias verificadas pela suíte de contrato, em TODO adaptador:
//  1. os bytes de Payload chegam IDÊNTICOS aos publicados, sem recodificação;
//  2. os demais campos do evento entregue são lidos do envelope, não copiados
//     do struct publicado — o assinante enxerga o que trafegou no fio;
//  3. Publish sem Payload monta um envelope a partir do evento (nunca um
//     json.Marshal do ports.Event), preservando os campos de identificação;
//  4. Type é o assunto da publicação; assinatura filtra por assunto com a
//     semântica do NATS ("*" casa um token, ">" casa a cauda), e lista vazia
//     casa tudo;
//  5. entrega ao-menos-uma-vez, ordem NÃO garantida — o Handler precisa ser
//     idempotente;
//  6. o que foi publicado ANTES da assinatura é entregue quando o durável
//     aparece (retenção), senão a espinha de eventos só funcionaria com a ordem
//     de boot certa;
//  7. erro do Handler provoca reentrega; sucesso não;
//  8. mensagem ilegível é DESCARTADA com registro, não reentregue para sempre:
//     uma mensagem venenosa não pode travar a fila;
//  9. Publish é seguro para uso concorrente; evento sem Type é recusado.
//
// FORA da porta, de propósito: desduplicação. O JetStream desduplica por ID
// dentro de uma janela; o adaptador em memória não desduplica. Como a entrega é
// ao-menos-uma-vez em ambos, consumidor idempotente é obrigatório de qualquer
// jeito — prometer dedup seria prometer o que só um adaptador cumpre.
type EventBus interface {
	Publish(ctx context.Context, e Event) error
	Subscribe(ctx context.Context, stream, durable string, subjects []string, h Handler) error
	Close() error
}

// ───────────────────────── SandboxLauncher ─────────────────────────

// IsolationTier é o nível de isolamento do substrato onde a demanda executa.
//
// É DECLARADO, nunca presumido. Quem provisiona diz qual quer; o launcher
// entrega EXATAMENTE aquele ou recusa. Não existe degradação silenciosa: um
// sandbox que pediu microVM e recebeu container continua parecendo saudável, e
// a diferença só aparece no dia do incidente. RuntimeClass de Kata falta na
// maioria das distribuições (spec do substrato §2 e R-4) — por isso a ausência
// precisa virar recusa com mensagem, não um nível a menos sem aviso.
type IsolationTier string

const (
	TierUnspecified    IsolationTier = ""
	TierHardware       IsolationTier = "hardware"        // Kata/Firecracker — microVM
	TierKernelEmulated IsolationTier = "kernel_emulated" // gVisor / Edera
	TierNamespace      IsolationTier = "namespace"       // container com securityContext estrito
)

func ValidIsolationTier(t IsolationTier) bool {
	switch t {
	case TierHardware, TierKernelEmulated, TierNamespace:
		return true
	}
	return false
}

// SandboxWorkspacePath é onde o workspace da demanda é montado DENTRO do
// sandbox, idêntico em todo adaptador.
//
// É constante da PORTA, e não campo da spec, porque é o único caminho cuja
// sobrevivência à suspensão é prometida. Se cada adaptador escolhesse o seu,
// "o trabalho sobrevive ao suspender" viraria promessa que depende de qual
// implantação atendeu a chamada — que é exatamente o tipo de diferença que a
// suíte de contrato existe para não deixar passar.
const SandboxWorkspacePath = "/workspace"

// SandboxHandle identifica um sandbox JÁ provisionado. O ID é do domínio: o
// adaptador nunca inventa identidade, só a carimba no que cria.
type SandboxHandle struct {
	ID        string
	Namespace string // dop-<id-curto> — um por demanda (spec §1)
}

// SandboxSpec é tudo o que o substrato precisa para materializar um sandbox.
// Repare no que NÃO está aqui: nada de kubeconfig, socket, runtimeClassName,
// nome de imagem de registry interno ou limite de cgroup. Isso é vocabulário de
// fornecedor e mora do lado de lá da porta.
type SandboxSpec struct {
	SandboxHandle
	AccountID string
	DemandID  string
	Tier      IsolationTier
	Image     string
	// Command vazio = entrypoint da imagem. Existe porque o substrato precisa
	// ser exercitável com uma imagem genérica na suíte de contrato — sem ele,
	// provar as garantias exigiria uma imagem de devbox publicada, e a suíte
	// deixaria de rodar no laptop de quem mexe no adaptador.
	Command []string
	Env     map[string]string
}

// SandboxPhase é o que o SUBSTRATO enxerga. Não é o estado do domínio: aqui não
// existe "destruído", porque para o launcher destruído e nunca existido são a
// mesma coisa — a memória de um sandbox destruído vive no Postgres.
type SandboxPhase string

const (
	PhaseProvisioning SandboxPhase = "provisioning"
	PhaseActive       SandboxPhase = "active"
	PhaseSuspended    SandboxPhase = "suspended"
)

// SandboxEndpoint é uma porta publicada pela pilha da demanda.
//
// Não tem URL de propósito: a URL é `<serviço>--<demanda>.<domínio>` (spec §5),
// e o domínio do ingress é política da instalação, não fato do substrato. Se
// cada adaptador montasse a URL, a mesma regra de nomeação existiria em dois
// lugares e divergiria no primeiro dia em que o domínio mudasse.
type SandboxEndpoint struct {
	Name  string
	Port  int32
	State string // running | stopped
}

type SandboxStatus struct {
	// Phase e Tier são o que o substrato ESTÁ entregando agora — não o que foi
	// pedido. É essa distinção que torna "declarado, nunca presumido"
	// verificável depois do provisionamento, e não só no momento dele.
	Phase     SandboxPhase
	Tier      IsolationTier
	Endpoints []SandboxEndpoint
}

// LogQuery seleciona o que sair pelo Tail. Service nomeia um processo DENTRO do
// sandbox (contêiner do pod, serviço do compose); vazio = o processo principal.
type LogQuery struct {
	Service   string
	TailLines int  // 0 = tudo o que o substrato ainda guarda
	Follow    bool // false = devolve o que já existe e retorna
}

// LogLine é uma linha crua do substrato. Classificação de origem (app, test,
// infra) NÃO está aqui: é convenção do que roda dentro do sandbox, e mora no
// domínio, onde muda em um lugar só.
type LogLine struct {
	Service string
	// Stream é stdout ou stderr — quando o substrato separa os dois. O
	// Kubernetes não separa (funde tudo no log do contêiner) e devolve sempre
	// "stdout"; o Docker separa. Por isso a suíte de contrato não promete nada
	// sobre este campo: quem depender dele estará dependendo do adaptador.
	Stream string
	Text   string
	At     time.Time
}

// SandboxLauncher é o substrato onde a demanda executa: microVM ou contêiner
// com o agente, o workspace e um Docker interno (spec do substrato §1).
//
// Duas coisas vivem dentro de um sandbox, e a porta inteira gira em torno da
// diferença entre elas: a EXECUÇÃO, efêmera e barata de recriar, e o WORKSPACE,
// que é o trabalho da demanda e não se recria. Suspender derruba a primeira e
// preserva o segundo; destruir leva os dois e não volta.
//
// Garantias verificadas pela suíte de contrato, em TODO adaptador:
//
//  1. Launch entrega o tier PEDIDO ou falha. Status.Tier é sempre igual a
//     Spec.Tier quando o erro é nil — degradar em silêncio é proibido, e um
//     tier que o substrato não oferece vira KindPrecondition com mensagem
//     dizendo o que falta;
//  2. tier recusado NÃO deixa rastro: depois da recusa, Describe devolve
//     KindNotFound. Recusa que provisiona metade é pior que recusa nenhuma;
//  3. SupportedTiers responde o que ESTE substrato oferece agora — é o que
//     permite ao domínio recusar antes de gravar estado. Nunca devolve lista
//     vazia sem erro: substrato que não oferece nível nenhum é substrato
//     indisponível (KindUnavailable);
//  4. Launch é IDEMPOTENTE por SandboxHandle.ID: relançar a mesma spec devolve
//     o sandbox existente em vez de criar um segundo. Sem isso, um retry de
//     rede duplicaria microVM — e a conta chega no fim do mês;
//  5. Suspend derruba a execução e PRESERVA tudo o que está sob
//     SandboxWorkspacePath. Nada FORA desse caminho é prometido: o adaptador
//     k8s apaga o pod inteiro na suspensão e só o PVC sobrevive, enquanto o
//     Docker mantém a camada gravável do contêiner. Prometer o que só um
//     cumpre seria a abstração vazando;
//  6. Resume recria a execução SOBRE o workspace existente e devolve a fase
//     ativa. Depois de Resume, o que estava no workspace continua lá;
//  7. Suspend e Resume são idempotentes: suspender suspenso e retomar ativo
//     não erram e não mudam nada;
//  8. Destroy é IRREVERSÍVEL e idempotente: leva execução e workspace, e
//     destruir o que não existe devolve nil. Depois dele, Describe devolve
//     KindNotFound e Resume RECUSA — não há caminho de volta pela porta;
//  9. Describe, Suspend e Resume de sandbox inexistente devolvem KindNotFound.
//     Só Destroy trata ausência como sucesso, porque só nele a ausência é o
//     resultado desejado;
//  10. sandboxes coexistem sem interferência: operação em um jamais altera ou
//     revela o outro, mesmo com a mesma imagem e o mesmo comando (spec §1);
//  11. Tail entrega as linhas do sandbox e MORRE JUNTO com o chamador: com o
//     contexto cancelado, retorna sem erro e sem deixar goroutine viva. Com
//     Follow=false, retorna ao fim do que existe;
//  12. erro do emit interrompe o Tail e sobe — é como o servidor descobre que
//     o cliente sumiu.
//
// FORA da porta, de propósito:
//
//   - EXEC dentro do sandbox. O k8s exige upgrade de conexão (SPDY/WebSocket)
//     com semântica própria de stream; o Docker usa hijack de HTTP. O terminal
//     do dev é PTY do dop-app (spec §5) e não passa por aqui;
//   - SNAPSHOT/restauração de microVM. O suporte no Kata é limitado e o desenho
//     não depende dele (spec §3): entraria como capacidade que o domínio
//     acabaria assumindo existir;
//   - LIMITES de cpu/memória. Não mapeiam entre um cgroup do Docker e um
//     ResourceQuota de namespace com LimitRange sem virar o denominador comum
//     do primeiro fornecedor que inspirou a porta.
type SandboxLauncher interface {
	SupportedTiers(ctx context.Context) ([]IsolationTier, error)
	Launch(ctx context.Context, spec SandboxSpec) (*SandboxStatus, error)
	Suspend(ctx context.Context, h SandboxHandle) error
	Resume(ctx context.Context, spec SandboxSpec) (*SandboxStatus, error)
	Destroy(ctx context.Context, h SandboxHandle) error
	Describe(ctx context.Context, h SandboxHandle) (*SandboxStatus, error)
	Tail(ctx context.Context, h SandboxHandle, q LogQuery, emit func(LogLine) error) error
}

// ───────────────────────── Clock e IDs ─────────────────────────
// Pequenas, mas reais: é o que torna o domínio determinístico em teste.

// Clock é a ÚNICA fonte de "agora" do domínio.
//
// Garantias verificadas pela suíte de contrato, em TODO adaptador:
//  1. Now nunca devolve o instante zero;
//  2. Now devolve sempre UTC — instante sem fuso definido é ambiguidade que
//     vira erro de comparação quando o processo e o banco discordam de TZ;
//  3. Now não regride entre chamadas sucessivas;
//  4. seguro para uso concorrente.
//
// O domínio recebe a porta e NUNCA chama time.Now() por dentro, nem como
// fallback para clock nulo: o fallback desliga a porta sem ninguém perceber e
// devolve ao teste a dependência do relógio de parede que a porta existe para
// remover. Serviço que precisa de tempo EXIGE o relógio no construtor.
type Clock interface{ Now() time.Time }

type IDGenerator interface{ NewID() string }
