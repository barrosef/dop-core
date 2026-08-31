package knowledge_test

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/knowledge"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// O domínio é testável SEM banco, SEM storage e SEM serviço de embedding:
// os três são portas, e aqui entram duplos em memória. É o retorno prático da
// arquitetura hexagonal.
//
// Os duplos moram NESTE arquivo, e não em internal/adapter: o teste de
// arquitetura varre todo .go sob internal/domain, inclusive os _test.go, e
// importar adaptador daqui quebraria a fronteira que ele protege.

// ── seleção do pacote: a função pura que decide o que o agente sabe ──────────

func TestHerancaDeRegras(t *testing.T) {
	regras := []knowledge.Artifact{
		{Scope: knowledge.AccountScope("a1"), Kind: knowledge.KindRule, Name: "branches", Body: "regra da casa"},
		{Scope: knowledge.AccountScope("a1"), Kind: knowledge.KindRule, Name: "segredos", Body: "nada em texto puro"},
		{Scope: knowledge.WorkspaceScope("a1", "w1"), Kind: knowledge.KindRule, Name: "testes", Body: "regra do workspace"},
		{Scope: knowledge.ProjectScope("a1", "p1"), Kind: knowledge.KindRule, Name: "branches", Body: "regra do projeto"},
		{Scope: knowledge.AccountScope("a1"), Kind: knowledge.KindRule, Name: "vazia", Body: "  "},
	}
	got := knowledge.ResolveRules(regras)

	// O mais específico GANHA do mais geral (a regra "branches" do projeto
	// substitui a da conta), o que não foi substituído continua valendo, e a
	// lista sai do específico para o geral — é o que faz o corte por orçamento
	// sacrificar primeiro a regra genérica.
	esperado := []string{"regra do projeto", "regra do workspace", "nada em texto puro"}
	if len(got) != len(esperado) {
		t.Fatalf("regras resolvidas = %v, esperado %v", got, esperado)
	}
	for i := range esperado {
		if got[i] != esperado[i] {
			t.Errorf("regra %d = %q, esperado %q", i, got[i], esperado[i])
		}
	}
	for _, r := range got {
		if r == "regra da casa" {
			t.Error("a regra da conta não pode conviver com a do projeto de mesmo nome — herança é substituição")
		}
	}
}

// TestSelecaoRespeitaOOrcamento é o teste central deste domínio: o pacote é
// SELECIONADO, não despejado (ADR-0012).
func TestSelecaoRespeitaOOrcamento(t *testing.T) {
	orc := knowledge.Budget{Total: 400, FindingShare: 0.2, IndexShare: 0.3, MemoryShare: 0.3}

	cand := knowledge.Candidates{
		Rules: []knowledge.Artifact{
			{Scope: knowledge.ProjectScope("a1", "p1"), Kind: knowledge.KindRule,
				Name: "branches", Body: texto(200)}, // ~50 tokens
		},
		Findings: []knowledge.Finding{
			{ID: "f1", Title: "achado", Summary: texto(100)},
			{ID: "f2", Title: "achado grande", Summary: texto(4000)}, // não cabe na cota
		},
		Index: []knowledge.Artifact{
			{ID: "i2", Kind: knowledge.KindIndex, Name: "beta", Body: texto(120), EstTokens: 30},
			{ID: "i1", Kind: knowledge.KindIndex, Name: "alfa", Body: texto(120), EstTokens: 30},
		},
		Memories: []knowledge.ScoredArtifact{
			{Score: 0.4, Artifact: knowledge.Artifact{ID: "m2", Kind: knowledge.KindMemory, Name: "b", EstTokens: 60}},
			{Score: 0.9, Artifact: knowledge.Artifact{ID: "m1", Kind: knowledge.KindMemory, Name: "a", EstTokens: 60}},
			{Score: 0.2, Artifact: knowledge.Artifact{ID: "m3", Kind: knowledge.KindMemory, Name: "c", EstTokens: 60}},
		},
	}

	p := knowledge.SelectPackage(orc, cand)

	if p.EstimatedTokens > orc.Total {
		t.Fatalf("o pacote estourou o orçamento: %d > %d", p.EstimatedTokens, orc.Total)
	}
	if len(p.Rules) != 1 {
		t.Errorf("a regra do projeto tem de entrar antes de tudo, veio %d", len(p.Rules))
	}
	// Achado grande não cabe na cota da camada: fica de fora, e o que fica de
	// fora é CONTADO — "coube" e "coube jogando metade fora" são fatos
	// diferentes.
	if len(p.Findings) != 1 || p.Dropped.Findings != 1 {
		t.Errorf("achados = %d, descartados = %d; esperado 1 e 1", len(p.Findings), p.Dropped.Findings)
	}
	// Índice sai em ordem estável de nome, não na ordem em que a consulta
	// devolveu.
	if len(p.Index) != 2 || p.Index[0].Name != "alfa" {
		t.Errorf("índice fora de ordem estável: %+v", p.Index)
	}
	// Memória entra por relevância decrescente e a cota corta o resto.
	if len(p.Memories) == 0 || p.Memories[0].ID != "m1" {
		t.Fatalf("a memória mais relevante deveria vir primeiro: %+v", p.Memories)
	}
	if !p.Truncated() {
		t.Error("o pacote foi cortado e não se declarou truncado")
	}
	// O corte não pode pular o item relevante para encaixar um menos
	// relevante: curadoria não é problema da mochila.
	for i, m := range p.Memories {
		if i > 0 && m.ID < p.Memories[i-1].ID {
			t.Errorf("ordem de relevância violada pelo empacotamento: %+v", p.Memories)
		}
	}
}

// TestSelecaoEDeterministica protege o prefixo cacheado do prompt: mesma
// entrada, mesma saída, byte a byte (ADR-0012 §1).
func TestSelecaoEDeterministica(t *testing.T) {
	cand := knowledge.Candidates{
		Memories: []knowledge.ScoredArtifact{
			{Score: 0.5, Artifact: knowledge.Artifact{ID: "m2", Name: "b", EstTokens: 10}},
			{Score: 0.5, Artifact: knowledge.Artifact{ID: "m1", Name: "a", EstTokens: 10}},
		},
	}
	primeiro := knowledge.SelectPackage(knowledge.Budget{}, cand)
	// Entrada embaralhada, empate de score: o desempate por id tem de mandar.
	cand.Memories[0], cand.Memories[1] = cand.Memories[1], cand.Memories[0]
	segundo := knowledge.SelectPackage(knowledge.Budget{}, cand)

	if len(primeiro.Memories) != 2 || primeiro.Memories[0].ID != "m1" {
		t.Fatalf("desempate por id não aplicado: %+v", primeiro.Memories)
	}
	for i := range primeiro.Memories {
		if primeiro.Memories[i].ID != segundo.Memories[i].ID {
			t.Fatal("duas montagens da mesma demanda produziram ordens diferentes")
		}
	}
	if primeiro.EstimatedTokens != segundo.EstimatedTokens {
		t.Error("a medição do pacote não é determinística")
	}
}

func TestOrcamentoZeradoUsaOPadrao(t *testing.T) {
	// Orçamento zero é "use o padrão", nunca "não cabe nada": um agente que
	// nasce cego por engano de configuração é o pior default possível.
	p := knowledge.SelectPackage(knowledge.Budget{}, knowledge.Candidates{
		Rules: []knowledge.Artifact{{Kind: knowledge.KindRule, Name: "r", Body: "vale"}},
	})
	if len(p.Rules) != 1 {
		t.Fatalf("orçamento zerado deveria cair no padrão, e a regra ficou de fora")
	}
	if p.Budget != knowledge.DefaultBudget().Total {
		t.Errorf("teto = %d, esperado o padrão %d", p.Budget, knowledge.DefaultBudget().Total)
	}
}

// ── montagem do pacote pelo serviço ──────────────────────────────────────────

func TestBuildContextPackageCortaPorOrcamentoEMede(t *testing.T) {
	repo := novoRepo()
	repo.add(regra("a1", "p1", "branches", "PR sempre contra release"))
	repo.add(indice("a1", "p1", "dop-core", 40))
	repo.add(indice("a1", "p1", "dop-app", 40))
	for _, id := range []string{"m1", "m2", "m3"} {
		m := memoria("a1", "p1", id, "lição sobre timeout", 80)
		m.ID = id
		repo.add(m)
	}
	demandas := &demandasFalsas{ctx: &knowledge.DemandContext{
		DemandID: "d1", ProjectID: "p1", Title: "corrigir timeout",
		Repos:    []string{"dop-core"},
		Findings: []knowledge.Finding{{ID: "f1", Title: "o pool estoura", Summary: "no pgbouncer"}},
	}}

	// Orçamento apertado de propósito: é o corte que este teste observa.
	svc := knowledge.NewService(repo, novoStorage(), demandas, nil, relogioFixo{},
		knowledge.Budget{Total: 200, FindingShare: 0.2, IndexShare: 0.3, MemoryShare: 0.3})

	pkg, err := svc.BuildContextPackage(ctxDe("a1"), "d1", knowledge.Budget{})
	if err != nil {
		t.Fatalf("montagem falhou: %v", err)
	}
	if pkg.EstimatedTokens > 200 {
		t.Fatalf("o pacote estourou o teto: %d", pkg.EstimatedTokens)
	}
	// O que cresce com a DEMANDA: só o índice do repositório que ela toca.
	if len(pkg.Index) != 1 || pkg.Index[0].Name != "dop-core" {
		t.Errorf("índice deveria ter só o repo da demanda, veio %+v", pkg.Index)
	}
	// Memória é a camada que o teto corta primeiro.
	if len(pkg.Memories) >= 3 {
		t.Errorf("com teto de 200 tokens as três memórias não cabem: %+v", pkg.Memories)
	}
	if !pkg.Truncated() || pkg.Dropped.Memories == 0 {
		t.Error("o corte aconteceu e não foi contabilizado")
	}
	// A medição da montagem é EVENTO, não impressão (ADR-0009 §3).
	if len(repo.medicoes) != 1 {
		t.Fatalf("a montagem deveria emitir exatamente uma medição, veio %d", len(repo.medicoes))
	}
	m := repo.medicoes[0]
	if m.EstimatedTokens != pkg.EstimatedTokens || m.Budget != 200 || !m.Dropped.Any() {
		t.Errorf("medição não reflete a montagem: %+v", m)
	}
	// O instante vem do RELÓGIO da porta, não de time.Now() escondido.
	if !m.At.Equal(instanteFixo) {
		t.Errorf("medição carimbada fora do relógio da porta: %v", m.At)
	}
}

func TestPacoteExigeDemandaEConta(t *testing.T) {
	svc := knowledge.NewService(novoRepo(), novoStorage(),
		&demandasFalsas{}, nil, relogioFixo{}, knowledge.Budget{})

	if _, err := svc.BuildContextPackage(context.Background(), "d1", knowledge.Budget{}); err == nil {
		t.Error("requisição sem conta ativa deveria ser recusada")
	}
	if _, err := svc.BuildContextPackage(ctxDe("a1"), "  ", knowledge.Budget{}); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("demanda vazia deveria ser argumento inválido, veio %v", err)
	}
	if _, err := svc.BuildContextPackage(ctxDe("a1"), "d-inexistente", knowledge.Budget{}); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("demanda inexistente deveria ser not_found, veio %v", err)
	}
}

// ── busca de memória ─────────────────────────────────────────────────────────

// TestBuscaDeMemoriaIsolaPorConta é o teste que não pode faltar: conhecimento
// vazado entre contas é o pior defeito concebível nesta plataforma.
func TestBuscaDeMemoriaIsolaPorConta(t *testing.T) {
	repo := novoRepo()
	repo.add(memoria("a1", "p1", "nossa", "timeout no pgbouncer", 10))
	repo.add(memoria("a2", "p9", "da-outra-conta", "timeout no pgbouncer", 10))

	svc := knowledge.NewService(repo, novoStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})

	hits, err := svc.SearchMemory(ctxDe("a1"), "p1", "timeout no pgbouncer", 10)
	if err != nil {
		t.Fatalf("busca falhou: %v", err)
	}
	for _, h := range hits {
		if h.Artifact.AccountID() != "a1" {
			t.Fatalf("memória da conta %q vazou para a conta a1", h.Artifact.AccountID())
		}
	}
	if len(hits) != 1 {
		t.Fatalf("esperava só a memória da própria conta, veio %d", len(hits))
	}
	// A conta da consulta vem do CONTEXTO, nunca do chamador: um serviço que
	// aceitasse conta por parâmetro passaria neste cenário e falharia no
	// mundo real.
	if repo.ultimaBusca.AccountID != "a1" {
		t.Errorf("a busca foi emitida com conta %q", repo.ultimaBusca.AccountID)
	}
}

func TestBuscaSemanticaSoQuandoHaEmbedder(t *testing.T) {
	repo := novoRepo()
	repo.add(memoria("a1", "p1", "lição", "o pool estourava", 10))

	// Sem Embedder: caminho LEXICAL. Não é o pretendido, mas devolver nada
	// seria pior — o agente começaria do zero.
	semEmbedder := knowledge.NewService(repo, novoStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})
	if _, err := semEmbedder.SearchMemory(ctxDe("a1"), "p1", "pool", 5); err != nil {
		t.Fatalf("busca lexical falhou: %v", err)
	}
	if len(repo.ultimaBusca.Embedding) != 0 {
		t.Error("sem Embedder a consulta não pode carregar vetor")
	}

	comEmbedder := knowledge.NewService(repo, novoStorage(), &demandasFalsas{},
		embedderFalso{dim: knowledge.EmbeddingDim}, relogioFixo{}, knowledge.Budget{})
	if _, err := comEmbedder.SearchMemory(ctxDe("a1"), "p1", "pool", 5); err != nil {
		t.Fatalf("busca semântica falhou: %v", err)
	}
	if len(repo.ultimaBusca.Embedding) != knowledge.EmbeddingDim {
		t.Errorf("a consulta deveria levar o vetor, veio %d dimensões", len(repo.ultimaBusca.Embedding))
	}
	if repo.ultimaBusca.Text == "" {
		t.Error("o texto acompanha o vetor: o adaptador precisa dos dois caminhos")
	}
}

// Buscar com um embedder diferente do que gerou os vetores devolve resultado
// PLAUSÍVEL e errado — o pior modo de falha de uma busca. Melhor recusar.
func TestEmbedderComDimensaoErradaERecusado(t *testing.T) {
	repo := novoRepo()
	svc := knowledge.NewService(repo, novoStorage(), &demandasFalsas{},
		embedderFalso{dim: 768}, relogioFixo{}, knowledge.Budget{})
	if _, err := svc.SearchMemory(ctxDe("a1"), "p1", "pool", 5); err == nil {
		t.Fatal("vetor de dimensão incompatível deveria ser recusado")
	}
}

func TestBuscaSemConsultaERecusada(t *testing.T) {
	svc := knowledge.NewService(novoRepo(), novoStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})
	if _, err := svc.SearchMemory(ctxDe("a1"), "p1", "   ", 5); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("busca vazia deveria ser argumento inválido, veio %v", err)
	}
	if _, err := svc.SearchMemory(context.Background(), "p1", "x", 5); err == nil {
		t.Error("busca sem conta ativa deveria ser recusada")
	}
}

// ── escrita: onde o conteúdo mora ────────────────────────────────────────────

// TestArtefatoGrandeVaiParaOObjectStore prova a divisão que sustenta o custo
// de leitura da tabela: a linha guarda a REFERÊNCIA, nunca os bytes.
func TestArtefatoGrandeVaiParaOObjectStore(t *testing.T) {
	repo, storage := novoRepo(), novoStorage()
	svc := knowledge.NewService(repo, storage, &demandasFalsas{}, nil, relogioFixo{}, knowledge.Budget{})

	grande := []byte(texto(knowledge.InlineMaxBytes + 1))
	a, err := svc.PutArtifact(ctxDe("a1"), knowledge.PutInput{
		Kind: knowledge.KindIndex, ProjectID: "p1", Name: "dop-core", Content: grande,
	})
	if err != nil {
		t.Fatalf("escrita falhou: %v", err)
	}
	if a.Body != "" {
		t.Error("a linha guardou o conteúdo: um mapa de megabytes na coluna encarece TODA leitura da tabela")
	}
	if !a.Externalized() || a.ObjectRef == "" {
		t.Fatal("a linha deveria guardar a referência do ObjectStore")
	}
	if a.SizeBytes != len(grande) {
		t.Errorf("tamanho gravado = %d, esperado %d", a.SizeBytes, len(grande))
	}
	if len(storage.puts) != 1 {
		t.Fatalf("o conteúdo deveria ter ido para o storage, houve %d escritas", len(storage.puts))
	}
	put := storage.puts[0]
	if string(put.conteudo) != string(grande) {
		t.Error("o storage recebeu conteúdo diferente do enviado")
	}
	// O emulador de Storage do ambiente local PENDURA com application/json
	// (P-13). Este teste existe para que a troca do tipo não passe despercebida.
	if put.contentType != knowledge.ArtifactContentType {
		t.Errorf("Content-Type = %q, esperado %q", put.contentType, knowledge.ArtifactContentType)
	}
	if strings.HasPrefix(put.contentType, "application/json") {
		t.Error("application/json trava o emulador local sem mensagem de erro — ver P-13")
	}
	// A chave carrega a conta: referência de uma conta nunca coincide com a de
	// outra, nem para o mesmo artefato.
	if !strings.HasPrefix(put.ref.Key, "a1/") {
		t.Errorf("chave sem a conta no prefixo: %q", put.ref.Key)
	}

	// Artefato pequeno faz o caminho oposto: fica na linha, indexável.
	pequeno, err := svc.PutArtifact(ctxDe("a1"), knowledge.PutInput{
		Kind: knowledge.KindMemory, ProjectID: "p1", Name: "lição", Content: []byte("o pool estourava"),
	})
	if err != nil {
		t.Fatalf("escrita pequena falhou: %v", err)
	}
	if pequeno.Body == "" || pequeno.Externalized() {
		t.Error("artefato pequeno deveria ficar na linha, onde é indexável")
	}
	if len(storage.puts) != 1 {
		t.Error("artefato pequeno não pode ir ao storage")
	}
}

func TestRegraGrandeERecusada(t *testing.T) {
	svc := knowledge.NewService(novoRepo(), novoStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})
	// Regra é texto que o agente lê INTEIRO em todo pacote: se não cabe inline,
	// não é regra.
	_, err := svc.PutArtifact(ctxDe("a1"), knowledge.PutInput{
		Kind: knowledge.KindRule, ProjectID: "p1", Name: "manual",
		Content: []byte(texto(knowledge.InlineMaxBytes + 1)),
	})
	if errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("regra gigante deveria ser recusada, veio %v", err)
	}
}

func TestEscritaCarregaChaveDeIdempotencia(t *testing.T) {
	repo := novoRepo()
	svc := knowledge.NewService(repo, novoStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})

	in := knowledge.PutInput{Kind: knowledge.KindMemory, ProjectID: "p1",
		Name: "lição", Content: []byte("achado"), IdempotencyKey: "k-1"}
	if _, err := svc.PutArtifact(ctxDe("a1"), in); err != nil {
		t.Fatalf("escrita falhou: %v", err)
	}
	if len(repo.idems) != 1 || repo.idems[0].Key != "k-1" {
		t.Fatalf("a chave não chegou ao repositório: %+v", repo.idems)
	}
	primeiro := repo.idems[0].RequestHash
	if primeiro == "" {
		t.Fatal("a chave viaja com a assinatura do conteúdo, senão repetição e corrupção viram a mesma coisa")
	}
	// Mesmo conteúdo ⇒ mesma assinatura; conteúdo diferente ⇒ assinatura
	// diferente, e é isso que o banco transforma em conflito.
	if _, err := svc.PutArtifact(ctxDe("a1"), in); err != nil {
		t.Fatalf("repetição falhou: %v", err)
	}
	if repo.idems[1].RequestHash != primeiro {
		t.Error("a mesma escrita produziu assinaturas diferentes")
	}
	in.Content = []byte("outro achado")
	if _, err := svc.PutArtifact(ctxDe("a1"), in); err != nil {
		t.Fatalf("escrita alterada falhou: %v", err)
	}
	if repo.idems[2].RequestHash == primeiro {
		t.Error("conteúdo diferente com a mesma chave deveria mudar a assinatura")
	}
}

func TestEscritaValidaTipoEscopoEConteudo(t *testing.T) {
	svc := knowledge.NewService(novoRepo(), novoStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})
	casos := map[string]knowledge.PutInput{
		"tipo desconhecido": {Kind: "diagrama", Name: "x", Content: []byte("c")},
		"sem nome":          {Kind: knowledge.KindMemory, Name: "  ", Content: []byte("c")},
		"sem conteúdo":      {Kind: knowledge.KindMemory, Name: "x"},
	}
	for nome, in := range casos {
		if _, err := svc.PutArtifact(ctxDe("a1"), in); errs.KindOf(err) != errs.KindInvalid {
			t.Errorf("%s deveria ser argumento inválido, veio %v", nome, err)
		}
	}
	if _, err := svc.PutArtifact(context.Background(), knowledge.PutInput{
		Kind: knowledge.KindMemory, Name: "x", Content: []byte("c")}); err == nil {
		t.Error("escrita sem conta ativa deveria ser recusada")
	}
}

func TestEscopoDerivadoDoQueVeio(t *testing.T) {
	repo := novoRepo()
	svc := knowledge.NewService(repo, novoStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})

	semProjeto, err := svc.PutArtifact(ctxDe("a1"), knowledge.PutInput{
		Kind: knowledge.KindRule, Name: "branches", Content: []byte("regra da casa")})
	if err != nil {
		t.Fatalf("escrita falhou: %v", err)
	}
	if semProjeto.Scope.Level != knowledge.ScopeAccount {
		t.Errorf("sem projeto o escopo é da conta, veio %q", semProjeto.Scope.Level)
	}
	comProjeto, err := svc.PutArtifact(ctxDe("a1"), knowledge.PutInput{
		Kind: knowledge.KindRule, ProjectID: "p1", Name: "branches", Content: []byte("regra do projeto")})
	if err != nil {
		t.Fatalf("escrita falhou: %v", err)
	}
	if comProjeto.Scope.Level != knowledge.ScopeProject || comProjeto.Scope.ProjectID != "p1" {
		t.Errorf("escopo de projeto mal derivado: %+v", comProjeto.Scope)
	}
	// A conta vem SEMPRE do contexto — nunca do que o chamador mandou.
	if semProjeto.AccountID() != "a1" || comProjeto.AccountID() != "a1" {
		t.Error("a conta do artefato tem de vir do contexto da chamada")
	}
}

// ── leitura de índice e regras ───────────────────────────────────────────────

func TestReadIndexEListRules(t *testing.T) {
	repo := novoRepo()
	repo.add(indice("a1", "p1", "dop-core", 30))
	repo.add(regra("a1", "", "branches", "regra da casa"))
	repo.add(regra("a1", "p1", "branches", "regra do projeto"))
	repo.add(regra("a2", "p9", "branches", "regra de outra conta"))

	svc := knowledge.NewService(repo, novoStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{})

	a, err := svc.ReadIndex(ctxDe("a1"), "p1", "dop-core")
	if err != nil || a == nil {
		t.Fatalf("índice do repositório deveria ser encontrado: %v", err)
	}
	// Índice ausente é NotFound: índice que mente com confiança é pior que
	// índice que não existe (R-2), e vazio silencioso é a mesma mentira.
	if _, err := svc.ReadIndex(ctxDe("a1"), "p1", "dop-app"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("índice ausente deveria ser not_found, veio %v", err)
	}
	// Isolamento também na leitura.
	if _, err := svc.ReadIndex(ctxDe("a2"), "p1", "dop-core"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("índice de outra conta não pode ser lido, veio %v", err)
	}

	regras, err := svc.ListRules(ctxDe("a1"), "p1")
	if err != nil {
		t.Fatalf("listagem de regras falhou: %v", err)
	}
	if len(regras) != 1 || regras[0] != "regra do projeto" {
		t.Errorf("herança mal resolvida: %v", regras)
	}
	if _, err := svc.ListRules(ctxDe("a1"), ""); errs.KindOf(err) != errs.KindInvalid {
		t.Error("regras de um projeto exigem o projeto")
	}
}

// ── montagem do serviço ──────────────────────────────────────────────────────

// Erro de MONTAGEM aparece no boot, não às três da manhã.
func TestServicoRecusaPortasObrigatorias(t *testing.T) {
	casos := map[string]func(){
		"sem repositório": func() {
			knowledge.NewService(nil, novoStorage(), &demandasFalsas{}, nil, relogioFixo{}, knowledge.Budget{})
		},
		"sem ObjectStore": func() {
			knowledge.NewService(novoRepo(), nil, &demandasFalsas{}, nil, relogioFixo{}, knowledge.Budget{})
		},
		"sem demandas": func() {
			knowledge.NewService(novoRepo(), novoStorage(), nil, nil, relogioFixo{}, knowledge.Budget{})
		},
		"sem relógio": func() {
			knowledge.NewService(novoRepo(), novoStorage(), &demandasFalsas{}, nil, nil, knowledge.Budget{})
		},
	}
	for nome, montar := range casos {
		t.Run(nome, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("montar o serviço %s deveria falhar no boot", nome)
				}
			}()
			montar()
		})
	}
	// O Embedder é o ÚNICO opcional: sem serviço de embedding a busca cai no
	// lexical, e o resto do domínio continua de pé.
	if svc := knowledge.NewService(novoRepo(), novoStorage(), &demandasFalsas{}, nil,
		relogioFixo{}, knowledge.Budget{}); svc == nil {
		t.Error("Embedder nulo é aceitável e documentado")
	}
}

// ═════════════════════════ duplos de teste ═══════════════════════════════════

var instanteFixo = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

type relogioFixo struct{}

func (relogioFixo) Now() time.Time { return instanteFixo }

func ctxDe(accountID string) context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: accountID, ActorID: "u1", ActorKind: ctxutil.ActorAgent})
}

// texto produz conteúdo de tamanho conhecido — o orçamento é medido em bytes.
func texto(n int) string { return strings.Repeat("a", n) }

func regra(account, project, name, body string) knowledge.Artifact {
	return knowledge.Artifact{
		Scope: escopo(account, project), Kind: knowledge.KindRule,
		Name: name, Body: body, EstTokens: knowledge.EstimateTokens(body),
	}
}

func indice(account, project, repo string, tokens int) knowledge.Artifact {
	return knowledge.Artifact{
		Scope: escopo(account, project), Kind: knowledge.KindIndex,
		Name: repo, Body: texto(tokens * 4), EstTokens: tokens,
	}
}

func memoria(account, project, name, body string, tokens int) knowledge.Artifact {
	return knowledge.Artifact{
		Scope: escopo(account, project), Kind: knowledge.KindMemory,
		Name: name, Body: body, EstTokens: tokens,
	}
}

func escopo(account, project string) knowledge.Scope {
	if project == "" {
		return knowledge.AccountScope(account)
	}
	return knowledge.ProjectScope(account, project)
}

// repoFalso reproduz o que o SQL faz: filtra por conta SEMPRE, resolve o
// alcance do escopo e devolve candidatas. Sem isso, o teste de isolamento
// estaria testando o duplo, e não o serviço.
type repoFalso struct {
	arts        []knowledge.Artifact
	idems       []knowledge.Idempotency
	medicoes    []knowledge.PackageMetrics
	ultimaBusca knowledge.MemoryQuery
	seq         int
}

func novoRepo() *repoFalso { return &repoFalso{} }

func (r *repoFalso) add(a knowledge.Artifact) {
	r.seq++
	if a.ID == "" {
		a.ID = "art-" + string(rune('a'+r.seq))
	}
	a.Version = 1
	r.arts = append(r.arts, a)
}

func (r *repoFalso) alcanca(a knowledge.Artifact, accountID, projectID string) bool {
	if a.Scope.AccountID != accountID {
		return false
	}
	switch a.Scope.Level {
	case knowledge.ScopeAccount:
		return true
	case knowledge.ScopeProject:
		return projectID != "" && a.Scope.ProjectID == projectID
	}
	return false
}

func (r *repoFalso) Put(_ context.Context, a *knowledge.Artifact, id knowledge.Idempotency) (*knowledge.Artifact, error) {
	r.idems = append(r.idems, id)
	r.seq++
	saved := *a
	saved.ID = "novo-" + string(rune('a'+r.seq))
	saved.Version = 1
	saved.CreatedAt, saved.UpdatedAt = instanteFixo, instanteFixo
	r.arts = append(r.arts, saved)
	return &saved, nil
}

func (r *repoFalso) IndexOf(_ context.Context, accountID, projectID, repo string) (*knowledge.Artifact, error) {
	for i := range r.arts {
		a := r.arts[i]
		if a.Kind == knowledge.KindIndex && a.Name == repo && r.alcanca(a, accountID, projectID) {
			return &a, nil
		}
	}
	return nil, nil
}

func (r *repoFalso) IndexFor(_ context.Context, accountID, projectID string, repos []string) ([]knowledge.Artifact, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	querido := map[string]bool{}
	for _, n := range repos {
		querido[n] = true
	}
	var out []knowledge.Artifact
	for i := range r.arts {
		a := r.arts[i]
		if a.Kind == knowledge.KindIndex && querido[a.Name] && r.alcanca(a, accountID, projectID) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r *repoFalso) RulesFor(_ context.Context, accountID, projectID string) ([]knowledge.Artifact, error) {
	var out []knowledge.Artifact
	for i := range r.arts {
		a := r.arts[i]
		if a.Kind == knowledge.KindRule && r.alcanca(a, accountID, projectID) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r *repoFalso) SearchMemory(_ context.Context, q knowledge.MemoryQuery) ([]knowledge.ScoredArtifact, error) {
	r.ultimaBusca = q
	var out []knowledge.ScoredArtifact
	for i := range r.arts {
		a := r.arts[i]
		if a.Kind != knowledge.KindMemory || !r.alcanca(a, q.AccountID, q.ProjectID) {
			continue
		}
		out = append(out, knowledge.ScoredArtifact{Artifact: a, Score: 1 / float32(len(out)+1)})
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func (r *repoFalso) RecordContextBuild(_ context.Context, _, _ string, m knowledge.PackageMetrics) error {
	r.medicoes = append(r.medicoes, m)
	return nil
}

// storageFalso satisfaz ports.ObjectStore e registra o que recebeu — inclusive
// o Content-Type, que é o detalhe que trava o ambiente local se mudar (P-13).
type storageFalso struct {
	puts []escritaNoStorage
	obj  map[string][]byte
}

type escritaNoStorage struct {
	ref         ports.ObjectRef
	conteudo    []byte
	contentType string
}

func novoStorage() *storageFalso { return &storageFalso{obj: map[string][]byte{}} }

func (s *storageFalso) Put(_ context.Context, ref ports.ObjectRef, content []byte, ct string) error {
	s.puts = append(s.puts, escritaNoStorage{ref: ref, conteudo: content, contentType: ct})
	s.obj[ref.Bucket+"/"+ref.Key] = content
	return nil
}

func (s *storageFalso) Get(_ context.Context, ref ports.ObjectRef) ([]byte, error) {
	c, ok := s.obj[ref.Bucket+"/"+ref.Key]
	if !ok {
		return nil, errs.NotFound("objeto")
	}
	return c, nil
}

func (s *storageFalso) Delete(_ context.Context, ref ports.ObjectRef) error {
	delete(s.obj, ref.Bucket+"/"+ref.Key)
	return nil
}

func (s *storageFalso) Stat(_ context.Context, ref ports.ObjectRef) (*ports.ObjectMeta, error) {
	c, ok := s.obj[ref.Bucket+"/"+ref.Key]
	if !ok {
		return nil, errs.NotFound("objeto")
	}
	return &ports.ObjectMeta{Size: int64(len(c)), ContentType: knowledge.ArtifactContentType, UpdatedAt: instanteFixo}, nil
}

func (s *storageFalso) SignedPutURL(context.Context, ports.ObjectRef, time.Duration) (string, error) {
	return "", errs.New(errs.KindUnavailable, "sem assinatura no duplo")
}

func (s *storageFalso) SignedGetURL(context.Context, ports.ObjectRef, time.Duration) (string, error) {
	return "", errs.New(errs.KindUnavailable, "sem assinatura no duplo")
}

// embedderFalso é DETERMINÍSTICO: o mesmo texto produz o mesmo vetor, sempre.
// Não há chamada externa em teste de domínio — e um vetor aleatório tornaria a
// ordem dos resultados irreproduzível.
type embedderFalso struct{ dim int }

func (e embedderFalso) Dimensions() int { return e.dim }

func (e embedderFalso) Embed(_ context.Context, text string) ([]float32, error) {
	sum := sha256.Sum256([]byte(text))
	v := make([]float32, e.dim)
	for i := range v {
		v[i] = float32(sum[i%len(sum)]) / 255
	}
	return v, nil
}

type demandasFalsas struct{ ctx *knowledge.DemandContext }

func (d *demandasFalsas) ContextOf(_ context.Context, _, demandID string) (*knowledge.DemandContext, error) {
	if d.ctx == nil || d.ctx.DemandID != demandID {
		return nil, nil
	}
	return d.ctx, nil
}
