package delivery_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// O domínio é testável SEM banco: repositório e demandas são portas, e aqui
// entram duplos em memória. Eles moram NESTE arquivo porque o teste de
// arquitetura reprova qualquer import de adaptador sob internal/domain,
// inclusive em arquivo _test.go.

// relogioFixo é o duplo do ports.Clock — instante fixo para que "a evidência é
// deste commit e terminou quando?" seja verificável por igualdade.
type relogioFixo struct{ t time.Time }

func (r relogioFixo) Now() time.Time { return r.t }

var agora = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

const (
	conta   = "acct-1"
	projeto = "proj-1"
	repo1   = "repo-1"
	repo2   = "repo-2"
	commit1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commit2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// ── duplo da porta de demandas ──────────────────────────────────────────────

// fakeDemands é a porta ESTREITA e SOMENTE LEITURA para o domínio de demanda.
// Repare que não há nada a espionar além de leitura: a entrega não tem como
// mexer no estado de uma demanda, e é isso que o teste da regra de ouro
// verifica estruturalmente.
type fakeDemands struct {
	demandas map[string]delivery.DemandInfo
	leituras int
}

func (f *fakeDemands) Demand(_ context.Context, accountID, id string) (*delivery.DemandInfo, error) {
	f.leituras++
	if accountID != conta {
		return nil, errs.Permission("demanda de outra conta")
	}
	d, ok := f.demandas[id]
	if !ok {
		return nil, errs.NotFound("demanda")
	}
	return &d, nil
}

var _ delivery.Demands = (*fakeDemands)(nil)

// ── duplo do repositório ────────────────────────────────────────────────────

type fakeRepo struct {
	runs       []delivery.VerificationRun
	prs        []delivery.PullRequest
	entries    []delivery.MergeQueueEntry
	directives map[string]*delivery.Directive
	// entregues guarda as instruções aplicadas por uma decisão — é onde o
	// teste confere que a coordenação é trabalho, nunca pausa.
	entregues []delivery.Instruction
	seq       int
}

func novoRepo() *fakeRepo {
	return &fakeRepo{directives: map[string]*delivery.Directive{}}
}

func (f *fakeRepo) id(prefixo string) string {
	f.seq++
	return fmt.Sprintf("%s-%d", prefixo, f.seq)
}

func (f *fakeRepo) RecordVerification(_ context.Context, r *delivery.VerificationRun, _ string) (*delivery.VerificationRun, error) {
	for i := range f.runs {
		x := f.runs[i]
		if x.AccountID == r.AccountID && x.DemandID == r.DemandID && x.RepoID == r.RepoID &&
			x.Commit == r.Commit && x.Kind == r.Kind && x.Suite == r.Suite {
			r.ID = x.ID
			r.Attempts = x.Attempts + 1
			f.runs[i] = *r
			return r, nil
		}
	}
	r.ID = f.id("run")
	f.runs = append(f.runs, *r)
	return r, nil
}

func (f *fakeRepo) EvidenceFor(_ context.Context, accountID, demandID, repoID, commit string) (delivery.Evidence, error) {
	ev := delivery.Evidence{DemandID: demandID, RepoID: repoID, Commit: commit}
	for _, r := range f.runs {
		if r.AccountID == accountID && r.DemandID == demandID && r.RepoID == repoID {
			ev.Runs = append(ev.Runs, r)
		}
	}
	return ev, nil
}

func (f *fakeRepo) ListPullRequests(_ context.Context, accountID string, filtro delivery.PRFilter) ([]delivery.PullRequest, error) {
	var out []delivery.PullRequest
	for _, pr := range f.prs {
		if pr.AccountID != accountID {
			continue
		}
		if filtro.DemandID != "" && pr.DemandID != filtro.DemandID {
			continue
		}
		if filtro.RepoID != "" && pr.RepoID != filtro.RepoID {
			continue
		}
		if filtro.OnlyOpen && pr.Merged {
			continue
		}
		out = append(out, pr)
	}
	return out, nil
}

func (f *fakeRepo) PullRequestByID(_ context.Context, accountID, id string) (*delivery.PullRequest, error) {
	for i := range f.prs {
		if f.prs[i].ID == id && f.prs[i].AccountID == accountID {
			return &f.prs[i], nil
		}
	}
	return nil, nil
}

func (f *fakeRepo) PullRequestOf(_ context.Context, accountID, demandID, repoID string) (*delivery.PullRequest, error) {
	for i := range f.prs {
		if f.prs[i].AccountID == accountID && f.prs[i].DemandID == demandID && f.prs[i].RepoID == repoID {
			return &f.prs[i], nil
		}
	}
	return nil, nil
}

// OpenPullRequest replica a trigger do banco: sem verde, não grava. O duplo
// precisa ser tão duro quanto o Postgres, senão o teste passa e a produção não.
func (f *fakeRepo) OpenPullRequest(ctx context.Context, pr *delivery.PullRequest, _ string) (*delivery.PullRequest, error) {
	ev, _ := f.EvidenceFor(ctx, pr.AccountID, pr.DemandID, pr.RepoID, pr.HeadCommit)
	if !ev.Green() {
		return nil, errs.Precondition("sem verde, sem PR: %s", ev.Reason())
	}
	pr.ID = f.id("pr")
	f.prs = append(f.prs, *pr)
	return pr, nil
}

func (f *fakeRepo) QueueOfRepo(_ context.Context, accountID, repoID string, incluirMergeados bool) ([]delivery.MergeQueueEntry, error) {
	var out []delivery.MergeQueueEntry
	for _, e := range f.entries {
		if e.AccountID != accountID || e.RepoID != repoID {
			continue
		}
		if !incluirMergeados && e.State.IsTerminal() {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (f *fakeRepo) QueueEntryByID(_ context.Context, accountID, id string) (*delivery.MergeQueueEntry, error) {
	for i := range f.entries {
		if f.entries[i].ID == id && f.entries[i].AccountID == accountID {
			return &f.entries[i], nil
		}
	}
	return nil, nil
}

// Enqueue imita as duas constraints do banco: um PR não entra duas vezes na
// fila do mesmo repositório, e a sequência é atribuída pelo repositório.
func (f *fakeRepo) Enqueue(_ context.Context, e *delivery.MergeQueueEntry, _ string) (*delivery.MergeQueueEntry, error) {
	var maior int64
	for _, x := range f.entries {
		if x.RepoID == e.RepoID {
			if x.PullRequestID == e.PullRequestID {
				return nil, errs.New(errs.KindAlreadyExists, "o PR já está na fila deste repositório")
			}
			if x.Seq > maior {
				maior = x.Seq
			}
		}
	}
	e.ID = f.id("mq")
	e.Seq = maior + 1
	f.entries = append(f.entries, *e)
	return e, nil
}

func (f *fakeRepo) SetQueueState(_ context.Context, accountID, entryID string, to delivery.QueueState, c *delivery.ConflictReport, _ string) (*delivery.MergeQueueEntry, error) {
	for i := range f.entries {
		if f.entries[i].ID == entryID && f.entries[i].AccountID == accountID {
			f.entries[i].State = to
			f.entries[i].Conflict = c
			return &f.entries[i], nil
		}
	}
	return nil, errs.NotFound("entrada da fila")
}

func (f *fakeRepo) ListDirectives(_ context.Context, accountID, projectID string) ([]delivery.Directive, error) {
	var out []delivery.Directive
	for _, d := range f.directives {
		if d.AccountID == accountID && d.ProjectID == projectID {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (f *fakeRepo) DirectiveByID(_ context.Context, accountID, id string) (*delivery.Directive, error) {
	d, ok := f.directives[id]
	if !ok || d.AccountID != accountID {
		return nil, nil
	}
	cp := *d
	return &cp, nil
}

func (f *fakeRepo) CreateDirective(_ context.Context, d *delivery.Directive, _ string) (*delivery.Directive, error) {
	d.ID = f.id("dir")
	cp := *d
	f.directives[d.ID] = &cp
	return d, nil
}

func (f *fakeRepo) DecideDirective(_ context.Context, accountID, id string, dec delivery.Decision, ins []delivery.Instruction, _ string) (*delivery.Directive, error) {
	d, ok := f.directives[id]
	if !ok || d.AccountID != accountID {
		return nil, errs.NotFound("diretriz")
	}
	d.Status = delivery.DirectiveDecided
	d.Decision = &dec
	f.entregues = append(f.entregues, ins...)
	// A ordem preferencial é a única coordenação que a entrega aplica sozinha:
	// mexe na PRIORIDADE da fila, nunca no estado da demanda.
	for _, i := range ins {
		if i.Action != delivery.DirectiveMergeOrder {
			continue
		}
		p, ok := i.Payload["priority"].(int)
		if !ok {
			continue
		}
		for k := range f.entries {
			if f.entries[k].DemandID == i.DemandID && !f.entries[k].State.IsTerminal() {
				f.entries[k].Priority = p
			}
		}
	}
	cp := *d
	return &cp, nil
}

var _ delivery.Repository = (*fakeRepo)(nil)

// ── cenário ─────────────────────────────────────────────────────────────────

func cenario() (*delivery.Service, *fakeRepo, *fakeDemands) {
	repo := novoRepo()
	dem := &fakeDemands{demandas: map[string]delivery.DemandInfo{
		"dem-0": {ID: "dem-0", ProjectID: projeto, Active: true},
		"dem-1": {ID: "dem-1", ProjectID: projeto, Active: true},
		"dem-2": {ID: "dem-2", ProjectID: projeto, Active: true},
	}}
	return delivery.NewService(repo, dem, relogioFixo{agora}), repo, dem
}

func comoAtor(id string) context.Context {
	return ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: conta, ActorID: id, ActorKind: ctxutil.ActorUser,
	})
}

func execucao(demanda, repoID, commit string, kind delivery.CheckKind, out delivery.Outcome) delivery.VerificationRun {
	return delivery.VerificationRun{
		DemandID: demanda, RepoID: repoID, Commit: commit,
		Kind: kind, Suite: string(kind) + "-suite", Outcome: out,
		Total: 10, Passed: 10, SandboxID: "sbx-" + demanda,
		LogRef: "logs/" + demanda + "/" + commit, EndedAt: agora,
	}
}

// verde registra o pacote mínimo que a ADR-0007 exige: aceitação aprovada mais
// parecer do crítico, os dois sobre o MESMO commit.
func verde(t *testing.T, svc *delivery.Service, ctx context.Context, demanda, repoID, commit string) {
	t.Helper()
	for _, k := range []delivery.CheckKind{delivery.CheckAcceptance, delivery.CheckCritic} {
		if _, err := svc.RecordVerification(ctx, execucao(demanda, repoID, commit, k, delivery.OutcomePassed), ""); err != nil {
			t.Fatalf("registrar %s: %v", k, err)
		}
	}
}

func abrePR(t *testing.T, svc *delivery.Service, ctx context.Context, demanda, repoID, commit string) *delivery.PullRequest {
	t.Helper()
	pr, err := svc.OpenPullRequest(ctx, delivery.OpenSpec{
		DemandID: demanda, RepoID: repoID, Repo: "acme/portal",
		SourceBranch: "feat/" + demanda, TargetBranch: "main", HeadCommit: commit,
	}, "idem-pr-"+demanda+"-"+repoID)
	if err != nil {
		t.Fatalf("abrir PR: %v", err)
	}
	return pr
}

// ── evidência de verde (ADR-0007) ───────────────────────────────────────────

func TestEvidenciaSemExecucaoNaoEVerde(t *testing.T) {
	ev := delivery.Evidence{DemandID: "dem-1", RepoID: repo1, Commit: commit1}
	if ev.Green() {
		t.Fatal("commit sem nenhuma execução não pode ser considerado verde")
	}
	// A recusa precisa DIZER o que falta — precondição sem qual precondição faz
	// o agente tentar de novo às cegas.
	if len(ev.Missing()) < 2 {
		t.Errorf("faltavam aceitação e crítico, veio: %v", ev.Missing())
	}
}

func TestEvidenciaDeOutroCommitNaoVale(t *testing.T) {
	ev := delivery.Evidence{
		DemandID: "dem-1", RepoID: repo1, Commit: commit2,
		Runs: []delivery.VerificationRun{
			execucao("dem-1", repo1, commit1, delivery.CheckAcceptance, delivery.OutcomePassed),
			execucao("dem-1", repo1, commit1, delivery.CheckCritic, delivery.OutcomePassed),
		},
	}
	// É o coração da ADR-0008: o verde de ontem não é o verde de agora.
	if ev.Green() {
		t.Fatal("evidência de outro commit não pode aprovar o commit em revisão")
	}
	if !strings.Contains(ev.Reason(), "não do commit") {
		t.Errorf("a recusa deveria apontar o commit divergente: %s", ev.Reason())
	}
}

func TestEvidenciaComFalhaNaoEVerde(t *testing.T) {
	ev := delivery.Evidence{
		DemandID: "dem-1", RepoID: repo1, Commit: commit1,
		Runs: []delivery.VerificationRun{
			execucao("dem-1", repo1, commit1, delivery.CheckAcceptance, delivery.OutcomePassed),
			execucao("dem-1", repo1, commit1, delivery.CheckCritic, delivery.OutcomePassed),
			execucao("dem-1", repo1, commit1, delivery.CheckE2E, delivery.OutcomeFailed),
		},
	}
	if ev.Green() {
		t.Fatal("uma execução reprovada derruba o verde inteiro")
	}
}

func TestEvidenciaExigeParecerDoCritico(t *testing.T) {
	ev := delivery.Evidence{
		DemandID: "dem-1", RepoID: repo1, Commit: commit1,
		Runs: []delivery.VerificationRun{
			execucao("dem-1", repo1, commit1, delivery.CheckAcceptance, delivery.OutcomePassed),
		},
	}
	if ev.Green() {
		t.Fatal("aceitação sem parecer do crítico não fecha o portão da ADR-0007 §3")
	}
}

func TestExecucaoSemRastroNaoEEvidencia(t *testing.T) {
	r := execucao("dem-1", repo1, commit1, delivery.CheckAcceptance, delivery.OutcomePassed)
	r.SandboxID, r.LogRef = "", ""
	if err := r.Validate(); err == nil {
		t.Error("'passou' sem sandbox nem log é palavra, não evidência")
	}
	r = execucao("dem-1", repo1, commit1, delivery.CheckAcceptance, delivery.OutcomePassed)
	r.Commit = ""
	if err := r.Validate(); err == nil {
		t.Error("execução sem commit não prova nada e deveria ser recusada")
	}
}

func TestPRNaoAbreSemVerde(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	_, err := svc.OpenPullRequest(ctx, delivery.OpenSpec{
		DemandID: "dem-1", RepoID: repo1, SourceBranch: "feat/x", HeadCommit: commit1,
	}, "idem-1")
	if err == nil || errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("sem verde, sem PR (ADR-0007); erro veio: %v", err)
	}
	if !strings.Contains(err.Error(), "aceitação") {
		t.Errorf("a recusa deveria dizer o que falta: %v", err)
	}
}

// ── a fila recusa sem evidência (ADR-0007 + ADR-0008) ───────────────────────

func TestEnqueueSemPRERecusado(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	_, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")
	if err == nil || errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("demanda sem PR não entra na fila; erro veio: %v", err)
	}
}

func TestEnqueueSemEvidenciaDeVerdeERecusado(t *testing.T) {
	svc, repo, _ := cenario()
	ctx := comoAtor("ed")

	// PR aberto com o commit verde...
	verde(t, svc, ctx, "dem-1", repo1, commit1)
	pr := abrePR(t, svc, ctx, "dem-1", repo1, commit1)

	// ...e depois o branch andou: o PR agora aponta para um commit que ninguém
	// verificou. É exatamente o buraco que a re-verificação da ADR-0008 fecha.
	for i := range repo.prs {
		if repo.prs[i].ID == pr.ID {
			repo.prs[i].HeadCommit = commit2
		}
	}

	_, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")
	if err == nil {
		t.Fatal("a fila aceitou entrada sem evidência do commit atual")
	}
	if errs.KindOf(err) != errs.KindPrecondition {
		t.Fatalf("recusa deveria ser failed_precondition, veio %s: %v", errs.KindOf(err), err)
	}
	// A mensagem precisa dizer O QUE falta, não só que faltou.
	if !strings.Contains(err.Error(), "aceitação") || !strings.Contains(err.Error(), "crítico") {
		t.Errorf("a recusa não diz o que falta: %v", err)
	}
}

func TestEnqueueComAceitacaoReprovadaERecusado(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	verde(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)

	// Uma suíte nova reprova no MESMO commit: o verde deixa de existir.
	if _, err := svc.RecordVerification(ctx,
		execucao("dem-1", repo1, commit1, delivery.CheckE2E, delivery.OutcomeFailed), ""); err != nil {
		t.Fatalf("registrar reprovação: %v", err)
	}

	if _, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1"); err == nil {
		t.Fatal("a fila aceitou PR com execução reprovada no commit")
	}
}

func TestEnqueueComVerdeEntraNaFila(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	verde(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)

	e, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")
	if err != nil {
		t.Fatalf("com evidência de verde a entrada deveria ser aceita: %v", err)
	}
	if e.State != delivery.StateQueued || e.Seq == 0 {
		t.Errorf("entrada malformada: estado=%s seq=%d", e.State, e.Seq)
	}
}

func TestUmPRNaoEntraDuasVezesNaFilaDoMesmoRepo(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	verde(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)
	if _, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1"); err != nil {
		t.Fatalf("primeira entrada: %v", err)
	}
	if _, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q2"); err == nil {
		t.Fatal("o mesmo PR entrou duas vezes na fila do mesmo repositório")
	}
}

func TestFilaESemprePorRepositorio(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	for _, d := range []string{"dem-1", "dem-2"} {
		verde(t, svc, ctx, d, repo1, commit1)
		abrePR(t, svc, ctx, d, repo1, commit1)
		if _, err := svc.EnqueueMerge(ctx, repo1, d, "idem-"+d); err != nil {
			t.Fatalf("enfileirar %s: %v", d, err)
		}
	}
	verde(t, svc, ctx, "dem-0", repo2, commit2)
	abrePR(t, svc, ctx, "dem-0", repo2, commit2)
	if _, err := svc.EnqueueMerge(ctx, repo2, "dem-0", "idem-dem-0"); err != nil {
		t.Fatalf("enfileirar no repo 2: %v", err)
	}

	fila1, _ := svc.GetMergeQueue(ctx, repo1)
	fila2, _ := svc.GetMergeQueue(ctx, repo2)
	if len(fila1) != 2 || len(fila2) != 1 {
		t.Fatalf("as filas não são independentes por repositório: %d e %d", len(fila1), len(fila2))
	}
	// Cada fila numera do 1: a posição é dentro do repositório, não global.
	if fila1[0].Position != 1 || fila2[0].Position != 1 {
		t.Errorf("posições fora do repositório: %d e %d", fila1[0].Position, fila2[0].Position)
	}
}

// ── ordem determinística (ADR-0008) ─────────────────────────────────────────

// Duas entradas com a MESMA prioridade não podem empatar: a sequência por
// repositório desempata sempre. Fila com empate é fila cuja ordem muda entre
// dois refreshes do cockpit.
func TestOrdemDaFilaEDeterministicaSobEmpate(t *testing.T) {
	base := []delivery.MergeQueueEntry{
		{ID: "c", RepoID: repo1, Priority: delivery.DefaultPriority, Seq: 3, State: delivery.StateQueued},
		{ID: "a", RepoID: repo1, Priority: delivery.DefaultPriority, Seq: 1, State: delivery.StateQueued},
		{ID: "b", RepoID: repo1, Priority: delivery.DefaultPriority, Seq: 2, State: delivery.StateQueued},
	}
	esperado := []string{"a", "b", "c"}

	// Entrada embaralhada de várias formas produz SEMPRE a mesma saída.
	for _, entrada := range [][]delivery.MergeQueueEntry{
		{base[0], base[1], base[2]},
		{base[2], base[0], base[1]},
		{base[1], base[2], base[0]},
	} {
		fila := delivery.SortQueue(entrada)
		for i := range fila {
			if fila[i].ID != esperado[i] || fila[i].Position != int32(i+1) {
				t.Fatalf("ordem instável: %s na posição %d, esperado %s",
					fila[i].ID, fila[i].Position, esperado[i])
			}
		}
	}

	// Ordem TOTAL: para nenhum par distinto os dois lados podem ser falsos —
	// é isso que significa "não existe empate ambíguo".
	for i := range base {
		for j := range base {
			if i == j {
				continue
			}
			if !base[i].Before(base[j]) && !base[j].Before(base[i]) {
				t.Errorf("empate ambíguo entre %s e %s", base[i].ID, base[j].ID)
			}
		}
	}
}

func TestPrioridadeVemAntesDaChegada(t *testing.T) {
	// Diretriz de ordem preferencial (ADR-0015 §6) mexe na prioridade: quem
	// chegou depois pode mergear antes. Repare que ninguém PAROU: a demanda
	// que perdeu a vez continua correndo, só mergeia depois.
	fila := delivery.SortQueue([]delivery.MergeQueueEntry{
		{ID: "primeiro", Priority: delivery.DefaultPriority, Seq: 1},
		{ID: "urgente", Priority: 10, Seq: 2},
	})
	if fila[0].ID != "urgente" || fila[1].ID != "primeiro" {
		t.Fatalf("prioridade não foi respeitada: %s, %s", fila[0].ID, fila[1].ID)
	}
}

func TestFilaNaoMostraOQueJaMergeou(t *testing.T) {
	svc, repo, _ := cenario()
	ctx := comoAtor("ed")
	verde(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)
	e, _ := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")
	repo.entries[0].State = delivery.StateMerged

	fila, err := svc.GetMergeQueue(ctx, repo1)
	if err != nil {
		t.Fatalf("consultar fila: %v", err)
	}
	if len(fila) != 0 {
		t.Errorf("entrada %s mergeada continua ocupando a fila", e.ID)
	}
}

// ── conflito vira item de decisão humana (ADR-0008 §2) ──────────────────────

func TestConflitoEscalaComRelato(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")
	verde(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)
	e, _ := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")

	if _, err := svc.AdvanceQueue(ctx, e.ID, delivery.StateRebasing, "idem-r1"); err != nil {
		t.Fatalf("ir para rebase: %v", err)
	}
	// Relato vazio não serve para ninguém decidir — a caixa de atenção recebe
	// contexto, não alarme.
	if _, err := svc.ReportConflict(ctx, e.ID, delivery.ConflictReport{}, "idem-c0"); err == nil {
		t.Error("conflito sem arquivos nem descrição deveria ser recusado")
	}

	got, err := svc.ReportConflict(ctx, e.ID, delivery.ConflictReport{
		Files: []string{"internal/app/register.go"}, BaseCommit: commit2, Attempts: 3,
		Detail: "rebase falhou três vezes no mesmo hunk",
	}, "idem-c1")
	if err != nil {
		t.Fatalf("escalar conflito: %v", err)
	}
	if !got.State.NeedsHuman() {
		t.Error("conflito precisa marcar estado que alimenta a caixa de atenção")
	}
	if got.Conflict == nil || got.Conflict.ReportedAt.IsZero() {
		t.Error("o relato do conflito precisa ficar gravado, com quando")
	}
	// Conflito não é fim: resolvido, volta para o rebase.
	if !delivery.StateConflict.CanTransitionTo(delivery.StateRebasing) {
		t.Error("conflito resolvido deveria voltar para o rebase")
	}
}

func TestFilaNaoPulaAReverificacao(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")
	verde(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)
	e, _ := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")

	// "na fila → mergeado" pularia o rebase e a re-verificação, que é onde a
	// quebra semântica entre demandas paralelas aparece (ADR-0008).
	if _, err := svc.AdvanceQueue(ctx, e.ID, delivery.StateMerged, "idem-x"); err == nil {
		t.Fatal("a fila deixou mergear sem passar por rebase e re-verificação")
	}
}

// ── diretrizes (ADR-0015) ───────────────────────────────────────────────────

func diretrizExemplo() delivery.Directive {
	return delivery.Directive{
		ProjectID: projeto,
		Kind:      delivery.DirectiveCherryPick,
		Summary:   "a demanda 1 depende do contrato que a demanda 0 está escrevendo",
		Payload: map[string]any{
			"overlapping_files": []any{"internal/app/register.go"},
		},
		AffectedDemands: []string{"dem-0", "dem-1"},
		Recommended:     "cherry-pick",
		Options: []delivery.DirectiveOption{
			{
				Key:     "cherry-pick",
				Summary: "quando a 0 commitar o contrato, a 1 faz cherry-pick e segue",
				Instructions: []delivery.Instruction{{
					DemandID: "dem-1", Action: delivery.DirectiveCherryPick,
					When:    "demanda dem-0 commitou o contrato",
					Payload: map[string]any{"from_demand": "dem-0"},
				}},
			},
			{
				Key:     "ordem-de-merge",
				Summary: "as duas seguem; a 0 mergeia antes da 1",
				Instructions: []delivery.Instruction{{
					DemandID: "dem-1", Action: delivery.DirectiveMergeOrder,
					Payload: map[string]any{"priority": 200, "repo_id": repo1},
				}},
			},
		},
	}
}

func TestDiretrizPrecisaDeOpcoesERecomendacao(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")

	d := diretrizExemplo()
	d.Options = d.Options[:1]
	if _, err := svc.ProposeDirective(ctx, d, "idem-d0"); err == nil {
		t.Error("uma opção só é alarme com botão de OK, não provocação de decisão")
	}

	d = diretrizExemplo()
	d.Recommended = "inexistente"
	if _, err := svc.ProposeDirective(ctx, d, "idem-d0"); err == nil {
		t.Error("recomendação fora das opções deveria ser recusada")
	}
}

// A regra de ouro da ADR-0015 §5 escrita como TIPO: não existe valor no
// vocabulário que expresse parar uma demanda.
func TestVocabularioDeCoordenacaoNaoTemPausa(t *testing.T) {
	for _, tentativa := range []delivery.DirectiveKind{"pause", "block", "suspend", "wait", ""} {
		if delivery.ValidDirectiveKind(tentativa) {
			t.Errorf("%q não pode existir no vocabulário de coordenação", tentativa)
		}
		ins := delivery.Instruction{DemandID: "dem-1", Action: tentativa}
		if err := ins.Validate(); err == nil {
			t.Errorf("instrução %q deveria ser recusada", tentativa)
		}
	}

	svc, _, _ := cenario()
	ctx := comoAtor("ed")
	d := diretrizExemplo()
	d.Options[0].Instructions[0].Action = "pause"
	if _, err := svc.ProposeDirective(ctx, d, "idem-d0"); err == nil {
		t.Fatal("o domínio aceitou uma diretriz que pausaria a demanda")
	}
}

func TestDecidirExigeQuemEPorQue(t *testing.T) {
	svc, _, _ := cenario()
	ctx := comoAtor("ed")
	d, err := svc.ProposeDirective(ctx, diretrizExemplo(), "idem-d1")
	if err != nil {
		t.Fatalf("propor: %v", err)
	}

	// Sem motivo: recusado. Decisão sem porquê vira mágica invisível.
	if _, err := svc.DecideDirective(ctx, d.ID,
		map[string]any{"option": "cherry-pick"}, "idem-x1"); err == nil {
		t.Error("decidir sem motivo deveria ser recusado")
	}
	// Opção que não foi oferecida: recusada, dizendo quais existem.
	_, err = svc.DecideDirective(ctx, d.ID,
		map[string]any{"option": "cancelar-tudo", "rationale": "porque sim"}, "idem-x2")
	if err == nil || !strings.Contains(err.Error(), "cherry-pick") {
		t.Errorf("a recusa deveria listar as opções oferecidas: %v", err)
	}
	// Sem ator identificado: ninguém para registrar como autor da decisão.
	semAtor := ctxutil.Into(context.Background(), ctxutil.Call{AccountID: conta})
	if _, err := svc.DecideDirective(semAtor, d.ID,
		map[string]any{"option": "cherry-pick", "rationale": "ok"}, "idem-x3"); err == nil {
		t.Error("decisão sem ator identificado deveria ser recusada")
	}

	// Agente não decide a própria proposta: o techlead detecta e propõe, o dev
	// escolhe (ADR-0015 §4).
	comoAgente := ctxutil.Into(context.Background(), ctxutil.Call{
		AccountID: conta, ActorID: "thread-7", ActorKind: ctxutil.ActorAgent})
	if _, err := svc.DecideDirective(comoAgente, d.ID, map[string]any{
		"option": "cherry-pick", "rationale": "eu mesmo resolvo",
	}, "idem-x0"); err == nil || errs.KindOf(err) != errs.KindPermission {
		t.Errorf("agente não deveria decidir diretriz; erro veio: %v", err)
	}

	decidida, err := svc.DecideDirective(ctx, d.ID, map[string]any{
		"option": "cherry-pick", "rationale": "a 1 só precisa do contrato, não do resto",
	}, "idem-x4")
	if err != nil {
		t.Fatalf("decidir: %v", err)
	}
	if decidida.Decision == nil || decidida.Decision.DecidedBy != "ed" ||
		decidida.Decision.Rationale == "" || !decidida.Decision.DecidedAt.Equal(agora) {
		t.Fatalf("a decisão precisa registrar quem, o quê e por quê: %+v", decidida.Decision)
	}
	// Repetir a MESMA decisão é inócuo; mudar de ideia exige diretriz nova.
	if _, err := svc.DecideDirective(ctx, d.ID, map[string]any{
		"option": "cherry-pick", "rationale": "de novo",
	}, "idem-x4"); err != nil {
		t.Errorf("repetir a mesma decisão deveria ser inócuo: %v", err)
	}
	if _, err := svc.DecideDirective(ctx, d.ID, map[string]any{
		"option": "ordem-de-merge", "rationale": "mudei de ideia",
	}, "idem-x5"); err == nil {
		t.Error("reescrever uma decisão já tomada deveria ser conflito")
	}
}

// O teste que a regra de ouro pede: decidir uma diretriz NÃO interrompe a
// demanda que já está andando.
//
// Duas afirmações, e as duas importam. A primeira é de estado: a demanda 1
// continua ativa, e a entrada dela na fila continua onde estava. A segunda é
// estrutural: tudo o que a decisão produziu foi TRABALHO A FAZER — nenhuma
// instrução fora do vocabulário de coordenação, porque nenhuma poderia existir.
func TestDecidirDiretrizNaoInterrompeDemandaEmAndamento(t *testing.T) {
	svc, repo, dem := cenario()
	ctx := comoAtor("ed")

	// A demanda 1 está andando: PR verde aberto e entrada na fila.
	verde(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)
	entrada, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")
	if err != nil {
		t.Fatalf("enfileirar: %v", err)
	}
	if _, err := svc.AdvanceQueue(ctx, entrada.ID, delivery.StateRebasing, "idem-r1"); err != nil {
		t.Fatalf("avançar: %v", err)
	}

	// O techlead detecta a transversal com a demanda 0 e o dev decide.
	d, err := svc.ProposeDirective(ctx, diretrizExemplo(), "idem-d1")
	if err != nil {
		t.Fatalf("propor diretriz: %v", err)
	}
	if _, err := svc.DecideDirective(ctx, d.ID, map[string]any{
		"option": "cherry-pick", "rationale": "a 1 segue e faz cherry-pick quando a 0 commitar",
	}, "idem-x1"); err != nil {
		t.Fatalf("decidir: %v", err)
	}

	// 1. A demanda continua ativa — a entrega só LÊ demanda, não há como parar.
	if info, _ := dem.Demand(ctx, conta, "dem-1"); !info.Active {
		t.Error("a demanda parou por causa de uma transversal identificada (ADR-0015 §5)")
	}
	// 2. A entrada dela na fila continua exatamente onde estava.
	viva, _ := repo.QueueEntryByID(ctx, conta, entrada.ID)
	if viva.State != delivery.StateRebasing {
		t.Errorf("a decisão mexeu no andamento da demanda: estado virou %s", viva.State)
	}
	// 3. E o que a decisão entregou foi trabalho, não uma ordem de parar.
	if len(repo.entregues) == 0 {
		t.Fatal("a decisão não entregou coordenação nenhuma")
	}
	for _, ins := range repo.entregues {
		if err := ins.Validate(); err != nil {
			t.Errorf("instrução entregue fora do vocabulário de coordenação: %v", err)
		}
		if ins.Action == delivery.DirectiveCherryPick && ins.When == "" {
			t.Error("cherry-pick sem condição vira espera: a demanda precisa saber QUANDO aplicar")
		}
	}
}

// Ordem preferencial é o caso em que a diretriz TOCA a fila — e ainda assim não
// para ninguém: mexe na prioridade, o estado da demanda não muda.
func TestDiretrizDeOrdemReordenaSemParar(t *testing.T) {
	svc, repo, _ := cenario()
	ctx := comoAtor("ed")

	verde(t, svc, ctx, "dem-1", repo1, commit1)
	abrePR(t, svc, ctx, "dem-1", repo1, commit1)
	entrada, _ := svc.EnqueueMerge(ctx, repo1, "dem-1", "idem-q1")
	if _, err := svc.AdvanceQueue(ctx, entrada.ID, delivery.StateRebasing, "idem-r1"); err != nil {
		t.Fatalf("avançar: %v", err)
	}

	d, _ := svc.ProposeDirective(ctx, diretrizExemplo(), "idem-d1")
	if _, err := svc.DecideDirective(ctx, d.ID, map[string]any{
		"option": "ordem-de-merge", "rationale": "a 0 precisa entrar antes",
	}, "idem-x1"); err != nil {
		t.Fatalf("decidir: %v", err)
	}

	depois, _ := repo.QueueEntryByID(ctx, conta, entrada.ID)
	if depois.Priority != 200 {
		t.Errorf("a ordem preferencial não foi aplicada: prioridade %d", depois.Priority)
	}
	if depois.State != delivery.StateRebasing {
		t.Errorf("reordenar mudou o andamento da demanda: %s", depois.State)
	}
}

// ── isolamento por conta ────────────────────────────────────────────────────

func TestOperacaoSemContaAtivaERecusada(t *testing.T) {
	svc, _, _ := cenario()
	ctx := ctxutil.Into(context.Background(), ctxutil.Call{ActorID: "ed"})

	if _, err := svc.GetMergeQueue(ctx, repo1); err == nil {
		t.Error("consultar fila sem conta ativa deveria ser recusado")
	}
	if _, err := svc.EnqueueMerge(ctx, repo1, "dem-1", "k"); err == nil {
		t.Error("enfileirar sem conta ativa deveria ser recusado")
	}
	if _, err := svc.ListDirectives(ctx, projeto); err == nil {
		t.Error("listar diretrizes sem conta ativa deveria ser recusado")
	}
}

func TestServicoRecusaRelogioNulo(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("relógio nil precisa falhar no boot, não em produção")
		}
	}()
	delivery.NewService(novoRepo(), &fakeDemands{}, nil)
}
