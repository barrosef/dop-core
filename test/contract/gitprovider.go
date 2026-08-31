package contract

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// GitProviderEnv é o que ESTE provedor oferece para a suíte trabalhar.
//
// Existe pelo mesmo motivo de SandboxEnv: o que muda entre GitHub e GitLab não
// é comportamento — é o NOME das coisas. "org/repo" contra "grupo%2Fprojeto",
// branch que conflita, repositório que o token não enxerga. Tudo o mais é da
// suíte, para que os dois adaptadores sejam medidos com a MESMA régua.
//
// Campo de função vazio quer dizer "este ambiente não sabe produzir esse caso":
// o subteste é PULADO com registro, nunca em silêncio. É a mesma disciplina do
// `Mint` da suíte de identidade — contra um provedor de verdade não dá para
// fabricar um conflito sob demanda sem empurrar commits, e mentir sobre a
// cobertura é pior que admitir o buraco.
type GitProviderEnv struct {
	// Conectar monta uma conexão que fala pelo ator informado. Ator vazio =
	// o ator padrão do ambiente.
	Conectar func(t *testing.T, actorID string) delivery.GitProvider

	// Ator é o ator para quem a conexão padrão foi montada (garantia 14).
	Ator string

	// TokenSentinela é o token EXATO que a conexão padrão carrega. A suíte
	// varre toda saída de erro atrás dele (garantia 13). Vazio = a varredura é
	// pulada com aviso GRITADO: é a garantia cuja falha custa a conta inteira.
	TokenSentinela string

	// ConectarSemCredencial monta uma conexão com token inválido — o caso do
	// KindUnauthorized da garantia 11.
	ConectarSemCredencial func(t *testing.T) delivery.GitProvider

	// Repo é o repositório onde a suíte abre e mergeia PRs.
	Repo string
	// RepoInvisivel não existe, ou existe e o token não alcança. Os dois casos
	// precisam sair como KindNotFound (garantia 12).
	RepoInvisivel string

	// RepoComFilaNativa e RepoSemFilaNativa são repositórios cuja resposta de
	// HasNativeQueue é CONHECIDA. Vazio = subteste pulado.
	RepoComFilaNativa string
	RepoSemFilaNativa string
	// RepoFilaIlegivel é o repositório sobre o qual o adaptador NÃO consegue
	// olhar (sem permissão, recurso de plano ausente). É o caso da garantia 15:
	// precisa sair ERRO, nunca `false`.
	RepoFilaIlegivel string

	// Par devolve um par (origem, destino) NOVO a cada chamada, que integra
	// limpo. Novo a cada chamada é obrigatório: contra um provedor real a
	// segunda execução da suíte encontraria o PR da primeira, e o subteste de
	// idempotência passaria por acidente.
	Par func(t *testing.T) (origem, destino string)
	// ParConflitante devolve um par que o provedor RECUSA por conflito.
	ParConflitante func(t *testing.T) (origem, destino string)
	// ParBloqueado devolve um par cujo merge é recusado por motivo que NÃO é
	// conflito — pipeline rodando, aprovação faltando (garantia 8).
	ParBloqueado func(t *testing.T) (origem, destino string)
	// ParSemCommits devolve um par cuja origem não existe ou não tem nada a
	// integrar: o caso em que abrir PR precisa FALHAR, e falhar com erro (não
	// com um PR de ExternalID vazio).
	ParSemCommits func(t *testing.T) (origem, destino string)

	// Espera é quanto tolerar num rebase assíncrono.
	Espera time.Duration
}

// GitProviderSuite verifica as dezessete garantias documentadas na porta delivery.GitProvider.
//
// Disciplina da ADR-0001: uma porta com um adaptador só é palpite. GitHub e
// GitLab não têm UMA linha em comum — número de PR contra iid de MR, `merged`
// contra `state`, 422 contra 409 para o mesmo fato — e é só passando os dois por
// esta suíte que "trocar de provedor é fiação" deixa de ser promessa.
func GitProviderSuite(t *testing.T, name string, env func(t *testing.T) GitProviderEnv) {
	t.Run(name, func(t *testing.T) {
		e := env(t)
		if e.Espera <= 0 {
			e.Espera = 2 * time.Minute
		}
		conectar := func(t *testing.T) delivery.GitProvider {
			t.Helper()
			return e.Conectar(t, e.Ator)
		}

		// ── garantia 1 e 2: conflito é DADO, com Detail ──────────────────────

		t.Run("1_rebase_conflitado_e_dado_nao_erro", func(t *testing.T) {
			if e.ParConflitante == nil {
				t.Skip("este ambiente não sabe fabricar conflito — caso não verificável aqui")
			}
			p := conectar(t)
			origem, destino := e.ParConflitante(t)
			ctx, cancel := context.WithTimeout(context.Background(), e.Espera)
			defer cancel()

			// O PR PRECISA existir antes — e isso não é cerimônia do teste, é
			// uma descoberta sobre os dois provedores. Nenhum dos dois oferece
			// "rebase de branch": o GitHub reaplica o branch de um PR (por
			// GraphQL) e o GitLab reaplica o branch de um MR (por rota REST),
			// SEMPRE sobre o destino daquele PR/MR. `RebaseSpec` parece uma
			// operação de git e não é.
			abrirParaRebase(t, p, e, origem, destino)

			r, err := p.Rebase(ctx, delivery.RebaseSpec{
				RepoExternalID: e.Repo, Branch: origem, Onto: destino})
			if err != nil {
				t.Fatalf("CONFLITO VIROU ERRO: o fluxo da ADR-0008 §2 depende de "+
					"conflito chegar como dado para virar tarefa do agente e item da "+
					"caixa de atenção; erro vira retry de infra e some: %v", err)
			}
			if !r.Conflicted {
				t.Fatalf("o provedor recusou a reaplicação e o resultado veio limpo: %+v", r)
			}
			// Garantia 2: Files é best-effort (nenhum dos dois provedores publica
			// a lista), mas Detail é obrigatório — é o que a caixa de atenção
			// mostra para o humano decidir sem arqueologia.
			if strings.TrimSpace(r.Detail) == "" {
				t.Error("conflito sem Detail: a caixa de atenção receberia um alarme vazio")
			}
			if len(r.Files) > 0 {
				t.Logf("este provedor informou %d arquivo(s) em conflito: %v", len(r.Files), r.Files)
			} else {
				t.Log("nenhum arquivo listado — esperado: a lista não sai da API pública " +
					"de PR/MR de nenhum dos dois provedores (garantia 2)")
			}
		})

		t.Run("1b_rebase_limpo_traz_os_dois_commits", func(t *testing.T) {
			if e.Par == nil {
				t.Skip("ambiente sem fábrica de branches")
			}
			p := conectar(t)
			origem, destino := e.Par(t)
			ctx, cancel := context.WithTimeout(context.Background(), e.Espera)
			defer cancel()
			abrirParaRebase(t, p, e, origem, destino)

			r, err := p.Rebase(ctx, delivery.RebaseSpec{
				RepoExternalID: e.Repo, Branch: origem, Onto: destino})
			if err != nil {
				t.Fatalf("Rebase: %v", err)
			}
			if r.Conflicted {
				t.Fatalf("par que integra limpo veio como conflito: %+v", r)
			}
			// HeadCommit é o que a fila re-verifica na posição seguinte
			// (ADR-0008 §1). Sem ele o "verde é sempre sobre um estado do
			// código" perde o estado.
			if strings.TrimSpace(r.HeadCommit) == "" {
				t.Error("rebase limpo sem HeadCommit: a fila não teria sobre qual commit re-verificar")
			}
		})

		// ── garantias 3, 4 e 5: abrir PR ────────────────────────────────────

		t.Run("3_abrir_pr_e_idempotente_por_branch", func(t *testing.T) {
			if e.Par == nil {
				t.Skip("ambiente sem fábrica de branches")
			}
			p := conectar(t)
			origem, destino := e.Par(t)
			ctx := context.Background()

			spec := delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: origem, TargetBranch: destino,
				Title:   "demanda 1: primeira tentativa",
				Body:    "aceitação: 12/12; crítico: aprovado; trace: dop://t/1",
				ActorID: e.Ator,
			}
			primeiro, err := p.OpenPullRequest(ctx, spec)
			if err != nil {
				t.Fatalf("1ª abertura: %v", err)
			}

			// Garantia 4: título e corpo DIFERENTES na segunda chamada. Se a
			// idempotência fosse "sobrescreve", o pacote de evidência da
			// ADR-0007 §4 seria trocado por um retry de rede.
			spec.Title = "demanda 1: retry depois de um timeout"
			spec.Body = "corpo diferente, que NÃO pode substituir o pacote de evidência"
			segundo, err := p.OpenPullRequest(ctx, spec)
			if err != nil {
				t.Fatalf("REABERTURA VIROU ERRO: um timeout de rede numa frota de "+
					"agentes viraria item de atenção sobre um PR que foi aberto com "+
					"sucesso: %v", err)
			}
			if primeiro.ExternalID != segundo.ExternalID {
				t.Fatalf("DOIS PRs PARA O MESMO BRANCH: %q e %q",
					primeiro.ExternalID, segundo.ExternalID)
			}
			if primeiro.URL != segundo.URL {
				t.Errorf("mesma identidade, URLs diferentes: %q e %q", primeiro.URL, segundo.URL)
			}
			// Garantia 4, a metade observável pela porta: o PR devolvido é o
			// que já existia, com a data de criação original. A outra metade —
			// "nenhum pedido de atualização foi enviado" — só é observável do
			// lado do adaptador, e está no teste do duplo local.
			if !primeiro.CreatedAt.Equal(segundo.CreatedAt) {
				t.Errorf("o PR foi recriado ou reescrito: criado em %v, depois em %v",
					primeiro.CreatedAt, segundo.CreatedAt)
			}
		})

		t.Run("5_pr_devolvido_tem_identidade", func(t *testing.T) {
			if e.Par == nil {
				t.Skip("ambiente sem fábrica de branches")
			}
			p := conectar(t)
			origem, destino := e.Par(t)
			pr, err := p.OpenPullRequest(context.Background(), delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: origem, TargetBranch: destino,
				Title: "identidade", Body: "evidência", ActorID: e.Ator})
			if err != nil {
				t.Fatalf("OpenPullRequest: %v", err)
			}
			if strings.TrimSpace(pr.ExternalID) == "" {
				t.Error("PR sem ExternalID: é o que Merge recebe depois — sem ele o PR " +
					"aberto não tem como ser mergeado pela fila")
			}
			if strings.TrimSpace(pr.URL) == "" {
				t.Error("PR sem URL: é o único endereço que o humano da caixa de atenção abre")
			}
			if pr.TargetBranch != destino {
				t.Errorf("destino divergente: pedi %q, veio %q", destino, pr.TargetBranch)
			}
			if pr.CreatedAt.IsZero() {
				t.Error("PR sem data de criação")
			}
		})

		t.Run("5b_pr_sem_o_que_integrar_falha_com_erro", func(t *testing.T) {
			if e.ParSemCommits == nil {
				t.Skip("este ambiente não sabe fabricar um branch sem nada a integrar")
			}
			p := conectar(t)
			origem, destino := e.ParSemCommits(t)
			pr, err := p.OpenPullRequest(context.Background(), delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: origem, TargetBranch: destino,
				Title: "sem commits", ActorID: e.Ator})
			if err == nil {
				t.Fatalf("PR aberto sobre branch que não existe ou não tem o que integrar: %+v", pr)
			}
			// O ponto NÃO é o Kind, é a ausência de meio-termo: a porta não
			// pode devolver ProviderPR vazio com erro nil (o espelho da
			// garantia 8 de ObjectStore sobre SignedPutURL).
			if pr.ExternalID != "" {
				t.Errorf("erro E PR ao mesmo tempo: %+v", pr)
			}
		})

		// ── garantias 6, 7 e 8: merge ───────────────────────────────────────

		t.Run("6_merge_e_idempotente", func(t *testing.T) {
			if e.Par == nil {
				t.Skip("ambiente sem fábrica de branches")
			}
			p := conectar(t)
			origem, destino := e.Par(t)
			ctx := context.Background()
			pr, err := p.OpenPullRequest(ctx, delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: origem, TargetBranch: destino,
				Title: "merge idempotente", ActorID: e.Ator})
			if err != nil {
				t.Fatalf("OpenPullRequest: %v", err)
			}
			spec := delivery.MergeSpec{
				RepoExternalID: e.Repo, PRExternalID: pr.ExternalID, ActorID: e.Ator}

			primeiro, err := p.Merge(ctx, spec)
			if err != nil {
				t.Fatalf("1º Merge: %v", err)
			}
			if !primeiro.Merged {
				t.Fatalf("PR sem impedimento não mergeou: %+v", primeiro)
			}
			// Garantia 7.
			if strings.TrimSpace(primeiro.MergeCommit) == "" {
				t.Error("Merged=true sem MergeCommit: 'aceitei seu pedido' e 'está na main' " +
					"são fatos diferentes, e a fila libera a próxima posição pelo segundo")
			}
			if primeiro.MergedAtUnix == 0 {
				t.Error("merge confirmado sem instante")
			}

			// Garantia 6. Cumprir isto custa uma leitura extra: os DOIS
			// provedores recusam o PR já mergeado com o MESMO código HTTP que
			// usam para "há conflito".
			segundo, err := p.Merge(ctx, spec)
			if err != nil {
				t.Fatalf("REMERGE VIROU ERRO: a fila reprocessa a posição depois de "+
					"uma queda e precisa reconhecer o que já entrou: %v", err)
			}
			if !segundo.Merged {
				t.Errorf("PR já mergeado devolveu Merged=false: %+v", segundo)
			}
			if segundo.Conflicted {
				t.Errorf("PR já mergeado devolveu CONFLITO — é a ambiguidade do 405 "+
					"vazando pela porta: %+v", segundo)
			}
			if segundo.MergeCommit != primeiro.MergeCommit {
				t.Errorf("o commit de merge mudou entre duas leituras: %q e %q",
					primeiro.MergeCommit, segundo.MergeCommit)
			}
		})

		t.Run("8_nao_mergeou_sem_ser_conflito_e_legitimo", func(t *testing.T) {
			if e.ParBloqueado == nil {
				t.Skip("este ambiente não sabe fabricar PR bloqueado por motivo que não é conflito")
			}
			p := conectar(t)
			origem, destino := e.ParBloqueado(t)
			ctx := context.Background()
			pr, err := p.OpenPullRequest(ctx, delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: origem, TargetBranch: destino,
				Title: "bloqueado", ActorID: e.Ator})
			if err != nil {
				t.Fatalf("OpenPullRequest: %v", err)
			}
			r, err := p.Merge(ctx, delivery.MergeSpec{
				RepoExternalID: e.Repo, PRExternalID: pr.ExternalID, ActorID: e.Ator})
			if err != nil {
				t.Fatalf("bloqueio virou erro — 'ainda não pode mergear' é estado do "+
					"fluxo, não falha de infra: %v", err)
			}
			if r.Merged {
				t.Fatalf("o provedor recusou o merge e o resultado diz que mergeou: %+v", r)
			}
			if r.Conflicted {
				t.Fatal("bloqueio classificado como CONFLITO: 'conflito' viraria o balde " +
					"de tudo o que não mergeou, e a caixa de atenção chamaria um humano " +
					"para resolver um pipeline que ainda está rodando")
			}
			if strings.TrimSpace(r.Detail) == "" {
				t.Error("não mergeou, não conflitou e não disse por quê")
			}
		})

		// ── garantias 9 e 10: rebase é síncrono, e exige PR aberto ──────────────────────────

		t.Run("9_contexto_cancelado_nunca_vira_sem_conflito", func(t *testing.T) {
			if e.Par == nil {
				t.Skip("ambiente sem fábrica de branches")
			}
			p := conectar(t)
			origem, destino := e.Par(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // já nasce cancelado

			r, err := p.Rebase(ctx, delivery.RebaseSpec{
				RepoExternalID: e.Repo, Branch: origem, Onto: destino})
			if err == nil {
				t.Fatalf("contexto cancelado e o rebase respondeu assim mesmo: %+v — "+
					"Conflicted=false significaria 'não conflitou' quando o que houve "+
					"foi 'não sei'", r)
			}
			if k := errs.KindOf(err); k != errs.KindUnavailable {
				t.Errorf("cancelamento deveria ser %s, veio %s: %v", errs.KindUnavailable, k, err)
			}
		})

		// ── garantia 11: vocabulário do provedor não cruza ──────────────────

		t.Run("11_externalid_e_opaco_e_circula", func(t *testing.T) {
			if e.Par == nil {
				t.Skip("ambiente sem fábrica de branches")
			}
			p := conectar(t)
			origem, destino := e.Par(t)
			ctx := context.Background()
			pr, err := p.OpenPullRequest(ctx, delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: origem, TargetBranch: destino,
				Title: "opacidade", ActorID: e.Ator})
			if err != nil {
				t.Fatalf("OpenPullRequest: %v", err)
			}
			// O teste da opacidade é o CICLO: o que a porta devolveu volta para
			// ela e funciona, sem que a suíte precise saber se aquilo é um
			// número de PR do GitHub ou um iid de MR do GitLab. Uma suíte que
			// afirmasse formato estaria escrevendo o vocabulário de um dos dois
			// na porta.
			r, err := p.Merge(ctx, delivery.MergeSpec{
				RepoExternalID: e.Repo, PRExternalID: pr.ExternalID, ActorID: e.Ator})
			if err != nil {
				t.Fatalf("o ExternalID devolvido pela porta não foi aceito de volta por ela: %v", err)
			}
			if !r.Merged {
				t.Fatalf("ciclo do ExternalID não completou: %+v", r)
			}
		})

		// ── garantia 12: ausência, permissão e credencial ───────────────────

		t.Run("12_repositorio_invisivel_e_notfound", func(t *testing.T) {
			if e.RepoInvisivel == "" {
				t.Skip("ambiente sem repositório invisível declarado")
			}
			p := conectar(t)
			_, err := p.OpenPullRequest(context.Background(), delivery.OpenPRSpec{
				RepoExternalID: e.RepoInvisivel, SourceBranch: "x", TargetBranch: "main",
				Title: "não deveria abrir", ActorID: e.Ator})
			if err == nil {
				t.Fatal("PR aberto em repositório inexistente")
			}
			if k := errs.KindOf(err); k != errs.KindNotFound {
				t.Errorf("esperava %s, veio %s: %v", errs.KindNotFound, k, err)
			}
		})

		t.Run("12b_credencial_invalida_e_unauthorized", func(t *testing.T) {
			if e.ConectarSemCredencial == nil {
				t.Skip("este ambiente não sabe montar conexão com credencial inválida")
			}
			p := e.ConectarSemCredencial(t)
			_, err := p.HasNativeQueue(context.Background(), e.Repo)
			if err == nil {
				t.Fatal("credencial inválida foi aceita")
			}
			if k := errs.KindOf(err); k != errs.KindUnauthorized {
				t.Errorf("esperava %s — a decisão para quem opera é 'a credencial não "+
					"serve', e confundi-la com indisponibilidade manda a equipe caçar o "+
					"defeito no lugar errado. Veio %s: %v", errs.KindUnauthorized, k, err)
			}
		})

		// ── garantia 13: o token não aparece em lugar nenhum ────────────────

		t.Run("13_token_nao_vaza_em_erro_nem_em_texto", func(t *testing.T) {
			if e.TokenSentinela == "" {
				t.Skip("ATENÇÃO: ambiente sem token sentinela — a garantia cuja falha " +
					"entrega a conta inteira NÃO foi verificada aqui")
			}
			p := conectar(t)
			ctx := context.Background()

			// Erros de vários caminhos: cada um formata mensagem por conta
			// própria, e basta UM esquecer.
			var erros []error
			if e.RepoInvisivel != "" {
				_, err := p.HasNativeQueue(ctx, e.RepoInvisivel)
				erros = append(erros, err)
				_, err = p.OpenPullRequest(ctx, delivery.OpenPRSpec{
					RepoExternalID: e.RepoInvisivel, SourceBranch: "b", TargetBranch: "main",
					ActorID: e.Ator})
				erros = append(erros, err)
				_, err = p.Merge(ctx, delivery.MergeSpec{
					RepoExternalID: e.RepoInvisivel, PRExternalID: "1", ActorID: e.Ator})
				erros = append(erros, err)
				_, err = p.Rebase(ctx, delivery.RebaseSpec{
					RepoExternalID: e.RepoInvisivel, Branch: "b", Onto: "main"})
				erros = append(erros, err)
			}
			if e.ConectarSemCredencial != nil {
				ruim := e.ConectarSemCredencial(t)
				_, err := ruim.HasNativeQueue(ctx, e.Repo)
				erros = append(erros, err)
			}
			_, err := p.OpenPullRequest(ctx, delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: "b", TargetBranch: "main",
				ActorID: "outro-ator-qualquer"})
			erros = append(erros, err)

			vistos := 0
			for _, err := range erros {
				if err == nil {
					continue
				}
				vistos++
				if strings.Contains(err.Error(), e.TokenSentinela) {
					// Sem imprimir o erro: imprimi-lo colocaria o token no log
					// do próprio teste.
					t.Fatal("VAZAMENTO: a mensagem de erro carrega o token do provedor — " +
						"erro sobe para log, e token em log é credencial em repouso")
				}
			}
			if vistos == 0 {
				t.Fatal("nenhum erro foi provocado: a varredura não verificou nada")
			}

			// E o próprio adaptador, formatado. `%+v` lê campos NÃO EXPORTADOS
			// por reflexão e não consegue chamar o String() deles — é por isso
			// que o token vive num closure, e não num campo.
			for _, s := range []string{fmt.Sprintf("%v", p), fmt.Sprintf("%+v", p), fmt.Sprintf("%#v", p)} {
				if strings.Contains(s, e.TokenSentinela) {
					t.Fatal("VAZAMENTO: formatar o adaptador revela o token")
				}
			}
			t.Logf("%d mensagem(ns) de erro varrida(s) sem sinal do token", vistos)
		})

		// ── garantia 14: o ator é conferido ─────────────────────────────────

		t.Run("14_ator_diferente_do_da_conexao_e_recusado", func(t *testing.T) {
			if e.Ator == "" {
				t.Skip("ambiente sem ator declarado")
			}
			p := conectar(t)
			_, err := p.OpenPullRequest(context.Background(), delivery.OpenPRSpec{
				RepoExternalID: e.Repo, SourceBranch: "qualquer", TargetBranch: "main",
				Title: "em nome de outro", ActorID: e.Ator + "-impostor"})
			if err == nil {
				t.Fatal("PR aberto em nome de um ator que NÃO é o da credencial: o PR " +
					"sairia assinado por quem quer que seja o dono do token fiado, e a " +
					"ADR-0003 ('quem conduziu assina') viraria mentira silenciosa")
			}
			if k := errs.KindOf(err); k != errs.KindPermission {
				t.Errorf("esperava %s, veio %s: %v", errs.KindPermission, k, err)
			}
			// E o mesmo para o merge: assinar o merge é assinar também.
			_, err = p.Merge(context.Background(), delivery.MergeSpec{
				RepoExternalID: e.Repo, PRExternalID: "1", ActorID: e.Ator + "-impostor"})
			if k := errs.KindOf(err); err == nil || k != errs.KindPermission {
				t.Errorf("Merge em nome de outro ator não foi recusado (%v)", err)
			}
		})

		// ── garantias 15 e 16: fila nativa ──────────────────────────────────

		t.Run("15_fila_nativa_nao_inventa_resposta", func(t *testing.T) {
			p := conectar(t)
			ctx := context.Background()
			verificados := 0

			if e.RepoComFilaNativa != "" {
				ok, err := p.HasNativeQueue(ctx, e.RepoComFilaNativa)
				if err != nil {
					t.Errorf("repositório com fila nativa: %v", err)
				} else if !ok {
					t.Error("repositório COM fila nativa respondeu false: a fila do DOP " +
						"orquestraria por cima da fila do provedor e as duas mergeariam " +
						"o mesmo repositório (ADR-0008 §4)")
				}
				verificados++
			}
			if e.RepoSemFilaNativa != "" {
				ok, err := p.HasNativeQueue(ctx, e.RepoSemFilaNativa)
				if err != nil {
					t.Errorf("repositório sem fila nativa: %v", err)
				} else if ok {
					t.Error("repositório SEM fila nativa respondeu true: o DOP sairia da " +
						"frente e ninguém serializaria os merges")
				}
				verificados++
			}
			if e.RepoFilaIlegivel != "" {
				ok, err := p.HasNativeQueue(ctx, e.RepoFilaIlegivel)
				if err == nil {
					t.Errorf("o adaptador NÃO conseguiu olhar e respondeu %v assim mesmo. "+
						"`false` é uma AFIRMAÇÃO — 'pode orquestrar por cima' — e afirmá-la "+
						"sem ter olhado é o mesmo defeito de degradar isolamento em silêncio", ok)
				}
				if ok {
					t.Error("resposta true JUNTO com erro: quem ignorar o erro sai da frente da fila")
				}
				verificados++
			}
			if verificados == 0 {
				t.Skip("ambiente não declarou nenhum repositório com resposta conhecida de fila nativa")
			}
		})

		t.Run("16_fila_nativa_e_leitura_e_e_estavel", func(t *testing.T) {
			if e.RepoSemFilaNativa == "" {
				t.Skip("ambiente sem repositório de fila conhecida")
			}
			p := conectar(t)
			ctx := context.Background()
			a, err := p.HasNativeQueue(ctx, e.RepoSemFilaNativa)
			if err != nil {
				t.Fatalf("HasNativeQueue: %v", err)
			}
			b, err := p.HasNativeQueue(ctx, e.RepoSemFilaNativa)
			if err != nil {
				t.Fatalf("2ª HasNativeQueue: %v", err)
			}
			if a != b {
				t.Fatalf("perguntar mudou a resposta: %v e depois %v", a, b)
			}
		})

		// ── garantia 17: concorrência ───────────────────────────────────────

		t.Run("17_seguro_para_uso_concorrente", func(t *testing.T) {
			if e.Par == nil {
				t.Skip("ambiente sem fábrica de branches")
			}
			p := conectar(t)
			const n = 6
			var wg sync.WaitGroup
			ids := make([]string, n)
			erros := make([]error, n)
			origem, destino := e.Par(t)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					// TODAS as goroutines abrem o MESMO PR: além de exercitar a
					// corrida no cliente HTTP, é a idempotência da garantia 3
					// sob concorrência — que é como ela acontece de verdade
					// numa frota de agentes com retry.
					pr, err := p.OpenPullRequest(context.Background(), delivery.OpenPRSpec{
						RepoExternalID: e.Repo, SourceBranch: origem, TargetBranch: destino,
						Title: fmt.Sprintf("concorrente %d", i), ActorID: e.Ator})
					ids[i], erros[i] = pr.ExternalID, err
				}(i)
			}
			wg.Wait()
			for i, err := range erros {
				if err != nil {
					t.Fatalf("abertura concorrente %d falhou: %v", i, err)
				}
			}
			for i := 1; i < n; i++ {
				if ids[i] != ids[0] {
					t.Fatalf("a corrida criou PRs diferentes para o mesmo branch: %q e %q",
						ids[0], ids[i])
				}
			}
		})
	})
}

// abrirParaRebase garante o PR que os dois provedores exigem antes de reaplicar.
//
// Está aqui, e não no ambiente, porque a exigência é dos PROVEDORES e vale para
// os dois — é parte do que a suíte descobriu, não configuração de quem monta.
func abrirParaRebase(t *testing.T, p delivery.GitProvider, e GitProviderEnv, origem, destino string) {
	t.Helper()
	if _, err := p.OpenPullRequest(context.Background(), delivery.OpenPRSpec{
		RepoExternalID: e.Repo, SourceBranch: origem, TargetBranch: destino,
		Title: "pré-requisito do rebase", ActorID: e.Ator,
	}); err != nil {
		t.Fatalf("não foi possível preparar o PR que o rebase exige: %v", err)
	}
}

// ErroDeRebaseSemPR é a checagem de que a exigência acima é RECUSADA com
// mensagem, e não com um rebase silencioso sobre o lugar errado. Fica separada
// da suíte principal porque só faz sentido onde o ambiente sabe garantir que
// NÃO existe PR para o par — contra um provedor real, isso exige um branch
// virgem.
func GitProviderRebaseSemPR(t *testing.T, p delivery.GitProvider, e GitProviderEnv, origem, destino string) {
	t.Helper()
	_, err := p.Rebase(context.Background(), delivery.RebaseSpec{
		RepoExternalID: e.Repo, Branch: origem, Onto: destino})
	if err == nil {
		t.Fatal("reaplicou um branch sem PR aberto: nenhum dos dois provedores faz " +
			"isso, então o que quer que tenha acontecido não foi o que o domínio pediu")
	}
	if k := errs.KindOf(err); k != errs.KindPrecondition {
		t.Errorf("esperava %s, veio %s: %v", errs.KindPrecondition, k, err)
	}
}
