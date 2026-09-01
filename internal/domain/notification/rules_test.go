package notification

import (
	"testing"
	"time"
)

// A tabela é a política inteira. Estes testes verificam as propriedades que a
// tornam SUBSTITUÍVEL por dado (P-29) — não o conteúdo de cada linha, que muda.

func TestTodaLinhaTemNomeAcaoTipoEPorque(t *testing.T) {
	for _, r := range Rules() {
		if r.Name == "" {
			t.Errorf("regra sem nome: o nome entra na chave de idempotência, "+
				"e chave com pedaço vazio colide com a próxima regra sem nome: %+v", r)
		}
		if r.Action == "" {
			t.Errorf("regra %q sem ação: no P-29 a ação deixa de ser só e-mail, e "+
				"a chave já precisa carregá-la", r.Name)
		}
		if r.Kind == "" {
			t.Errorf("regra %q sem tipo: é o tipo que todo adaptador precisa resolver", r.Name)
		}
		if r.Why == "" {
			t.Errorf("regra %q sem justificativa: política sem porquê não é auditável — "+
				"ninguém consegue discordar de 'invite.created → e-mail'", r.Name)
		}
		switch r.Trigger {
		case TriggerEvent:
			if r.Event == "" {
				t.Errorf("regra %q é de evento e não diz qual", r.Name)
			}
		case TriggerAttentionBox:
			if r.Delay <= 0 {
				t.Errorf("regra %q é da caixa e não tem atraso: sem espera, um e-mail "+
					"por item torna a caixa de entrada inútil", r.Name)
			}
		default:
			t.Errorf("regra %q tem gatilho desconhecido %q", r.Name, r.Trigger)
		}
	}
}

func TestNomesDeRegraSaoUnicos(t *testing.T) {
	// Nome repetido é chave de idempotência repetida: a segunda regra do mesmo
	// evento seria descartada como duplicata da primeira — em silêncio, que é o
	// modo de falhar que a chave composta existe para evitar.
	vistos := map[string]bool{}
	for _, r := range Rules() {
		if vistos[r.Name] {
			t.Fatalf("duas regras chamadas %q", r.Name)
		}
		vistos[r.Name] = true
	}
}

func TestKindsVemDaTabelaESemRepeticao(t *testing.T) {
	ks := Kinds()
	if len(ks) == 0 {
		t.Fatal("nenhum tipo: a suíte de contrato do Mailer não provaria nada")
	}
	naTabela := map[Kind]bool{}
	for _, r := range Rules() {
		naTabela[r.Kind] = true
	}
	vistos := map[Kind]bool{}
	for _, k := range ks {
		if vistos[k] {
			t.Errorf("tipo %q repetido em Kinds()", k)
		}
		vistos[k] = true
		if !naTabela[k] {
			t.Errorf("tipo %q não vem de regra nenhuma: Kinds() virou lista à mão, e "+
				"lista à mão é o que deixa a suíte de contrato verde com template "+
				"faltando", k)
		}
	}
	for k := range naTabela {
		if !vistos[k] {
			t.Errorf("a regra declara o tipo %q e Kinds() não o devolve", k)
		}
	}
}

func TestSubjectsCobreTodosOsEventosDaTabela(t *testing.T) {
	// Assinatura a MENOS faz o evento nunca chegar — ninguém recebe e nada
	// falha. É a mesma armadilha de attention.Subjects, e aqui ela é derivada
	// justamente para não depender de alguém lembrar.
	assinados := map[string]bool{}
	for _, s := range Subjects() {
		assinados[s] = true
	}
	for _, r := range Rules() {
		if r.Trigger == TriggerEvent && !assinados[r.Event] {
			t.Errorf("a regra %q reage a %q e o consumidor não assina esse assunto",
				r.Name, r.Event)
		}
	}
}

func TestDigestRuleEhUnicaEAchadaPeloNome(t *testing.T) {
	r, ok := DigestRule()
	if !ok {
		t.Fatal("sem regra de resumo, a caixa nunca vira e-mail")
	}
	if r.Name == "" {
		t.Fatal("a regra de resumo precisa de nome: ele é gravado na chave de " +
			"idempotência, e uma varredura que assumisse posição na tabela gravaria " +
			"chave errada no dia em que alguém reordenasse o literal")
	}
	n := 0
	for _, c := range Rules() {
		if c.Trigger == TriggerAttentionBox {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d regras de caixa: DigestRule devolveria uma arbitrária", n)
	}
}

func TestApplyIgnoraOQueNaoEstaNaTabela(t *testing.T) {
	e := Event{ID: "ev-1", AccountID: "conta-1", Type: "dop.demand.stage.advanced",
		OccurredAt: time.Now(), Payload: map[string]any{}}
	if cs := Apply(e, semDestinatario); len(cs) != 0 {
		t.Fatalf("evento fora da tabela virou %d comando(s)", len(cs))
	}
}

func TestApplyIgnoraEventoSemConta(t *testing.T) {
	// `dop.identity.user.ensured` ocorre no primeiro login, antes de a conta
	// pessoal existir (migração 0003). Não há conta em nome de quem avisar.
	e := Event{ID: "ev-1", Type: EvInviteCreated,
		Payload: map[string]any{"email": "a@b.test"}}
	if cs := Apply(e, payloadEmail); len(cs) != 0 {
		t.Fatalf("evento sem conta virou %d comando(s)", len(cs))
	}
}

func TestApplyDoConviteMontaAChaveComposta(t *testing.T) {
	e := Event{
		ID: "ev-1", AccountID: "conta-1", Aggregate: "invite", AggregateID: "inv-1",
		Type: EvInviteCreated, OccurredAt: time.Now(),
		Payload: map[string]any{"email": "convidado@exemplo.test", "role": "member"},
	}
	cs := Apply(e, payloadEmail)
	if len(cs) != 1 {
		t.Fatalf("esperava 1 comando, veio %d", len(cs))
	}
	c := cs[0]
	if !c.Valid() {
		t.Fatalf("comando inválido: %+v", c)
	}
	ev, regra, acao := c.Key()
	if ev != "ev-1" || regra == "" || acao == "" {
		t.Fatalf("chave incompleta: (%q, %q, %q) — chave só pelo evento descartaria "+
			"a segunda ação do mesmo evento como duplicata, em silêncio", ev, regra, acao)
	}
	if c.Kind != KindInvite {
		t.Fatalf("tipo %q, esperava %q", c.Kind, KindInvite)
	}
	if len(c.Recipients) != 1 || c.Recipients[0].Email != "convidado@exemplo.test" {
		t.Fatalf("destinatário: %+v — o convidado AINDA NÃO É USUÁRIO, e o único "+
			"lugar onde o endereço dele existe é o payload", c.Recipients)
	}
	if c.Data["role"] != "member" {
		t.Fatalf("os dados do template não trouxeram `role`: %+v", c.Data)
	}
}

func TestApplySemDestinatarioNaoViraComando(t *testing.T) {
	// Sem destinatário não há o que disparar, e isso NÃO é erro: parar o
	// consumidor aqui atrasaria todas as notificações da fila.
	e := Event{ID: "ev-1", AccountID: "conta-1", Type: EvInviteCreated,
		Payload: map[string]any{"email": "isto-nao-e-endereco"}}
	if cs := Apply(e, payloadEmail); len(cs) != 0 {
		t.Fatalf("endereço inválido virou %d comando(s)", len(cs))
	}
}

func TestRulesDevolveCopia(t *testing.T) {
	// Política que o chamador consegue editar em memória deixa de ser política.
	rs := Rules()
	if len(rs) == 0 {
		t.Fatal("tabela vazia")
	}
	original := rs[0].Name
	rs[0].Name = "sabotado"
	if Rules()[0].Name != original {
		t.Fatal("Rules() devolveu a fatia interna: quem chama consegue reescrever a política")
	}
}

func semDestinatario(recipientSpec, Event) []Recipient { return nil }
