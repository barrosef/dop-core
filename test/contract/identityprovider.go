package contract

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// TokenSpec descreve o token que a suíte quer que ESTE emissor produza.
//
// A suíte não sabe montar token: cada emissor tem o seu formato de claim (o
// Firebase põe o provedor em `firebase.sign_in_provider`, o OIDC em `amr`), e o
// que importa é que o PRINCIPAL do outro lado saia igual. Por isso a suíte
// descreve a INTENÇÃO e o ambiente traduz.
type TokenSpec struct {
	Subject       string
	Email         string
	EmailVerified *bool // nil = a claim não vai no token
	Name          string
	Picture       string
	Providers     []string
	// Extra são claims que o emissor carrega e que o domínio NÃO pode ver.
	Extra map[string]any

	// Vazio = o valor correto. Preenchido = o token é emitido errado de
	// propósito.
	Issuer   string
	Audience string

	IssuedAt  time.Time
	NotBefore time.Time
	Expiry    time.Time // zero = uma hora no futuro

	// Casos de assinatura. Emissor que não sabe produzir devolve ok=false.
	WrongKey  bool // assinado por outra chave, com outro kid
	Unsigned  bool // "alg":"none"
	Symmetric bool // HS256 usando a chave pública como segredo
}

// IdentityEnv é o que ESTE emissor oferece para a suíte trabalhar.
type IdentityEnv struct {
	Provider ports.IdentityProvider

	// Mint devolve o token cru. ok=false quer dizer "este emissor não sabe
	// produzir esse caso" — o subteste é PULADO com registro, nunca em silêncio.
	Mint func(t *testing.T, s TokenSpec) (raw string, ok bool)

	// Unsigned marca emissor que não assina nada.
	//
	// Existe por causa de uma armadilha real: o emulador do Firebase emite
	// tokens com "alg":"none", e uma suíte que só rodasse contra ele passaria
	// verde sem provar UMA linha de verificação de assinatura — que é justamente
	// a parte cuja falha entrega a conta de qualquer usuário. Quando isto é
	// verdade, os casos de assinatura são pulados com aviso GRITADO, e a
	// cobertura precisa vir de outro ambiente do mesmo adaptador.
	Unsigned    bool
	UnsignedWhy string

	// Fetches conta idas ao emissor atrás de chave. nil = não observável (e
	// então as garantias 8 e parte da 7 não são verificáveis aqui).
	Fetches func() int
	// Rotate troca a chave de assinatura do emissor, como uma rotação de
	// verdade: a chave nova tem outro kid.
	Rotate func(t *testing.T)
	// MinRefresh é o freio de rebusca configurado neste adaptador.
	MinRefresh time.Duration
	// IssuerDown derruba o emissor.
	IssuerDown func(t *testing.T)
}

// IdentityProviderSuite verifica as onze garantias documentadas na porta
// ports.IdentityProvider.
//
// Disciplina da ADR-0001: uma porta com um adaptador só é palpite. O Firebase e
// um emissor OIDC genérico não compartilham nem o formato da chave pública
// (certificado X.509 de um lado, JWKS do outro) nem o vocabulário de claim — e é
// passar os dois pelas MESMAS onze verificações que transforma "trocar o
// provedor de identidade é fiação" em fato.
func IdentityProviderSuite(t *testing.T, name string, newEnv func(t *testing.T) IdentityEnv) {
	t.Run(name, func(t *testing.T) {
		ctx := context.Background()

		// mint aborta o subteste quando o emissor não sabe produzir o caso —
		// pulando com mensagem, que é o contrário de fingir que passou.
		mint := func(t *testing.T, env IdentityEnv, s TokenSpec) string {
			t.Helper()
			raw, ok := env.Mint(t, s)
			if !ok {
				t.Skipf("este emissor não sabe produzir o token pedido — caso não verificável aqui")
			}
			return raw
		}
		t.Run("1_token_improvavel_e_unauthorized", func(t *testing.T) {
			env := newEnv(t)

			// Estes a suíte monta sozinha: nenhum emissor precisa colaborar para
			// que lixo seja recusado.
			lixo := map[string]string{
				"vazio":                    "",
				"so_espaco":                "   ",
				"so_o_prefixo_bearer":      "Bearer ",
				"nao_e_jwt":                "isto-nao-e-um-token",
				"duas_partes":              "aaaabbbb.ccccdddd",
				"quatro_partes":            "aaaabbbb.ccccdddd.eeeeffff.gggghhhh",
				"base64_invalido":          "@@@@@@@@.@@@@@@@@.@@@@@@@@",
				"cabecalho_nao_e_json":     "bm90LWpzb24.bm90LWpzb24.YWJjZA",
				"tres_partes_vazias_com_h": "..",
			}
			for nome, raw := range lixo {
				t.Run(nome, func(t *testing.T) {
					p, err := env.Provider.VerifyToken(ctx, raw)
					requerRecusa(t, p, err, raw)
				})
			}

			agora := time.Now()
			casos := map[string]TokenSpec{
				"expirado":          {Subject: "s", Expiry: agora.Add(-2 * time.Hour)},
				"ainda_nao_valido":  {Subject: "s", NotBefore: agora.Add(2 * time.Hour)},
				"emitido_no_futuro": {Subject: "s", IssuedAt: agora.Add(2 * time.Hour)},
				"emissor_errado":    {Subject: "s", Issuer: "https://emissor-de-outra-instalacao.example"},
				"audiencia_errada":  {Subject: "s", Audience: "outra-aplicacao"},
				"sem_sujeito":       {},
			}
			for nome, spec := range casos {
				t.Run(nome, func(t *testing.T) {
					env := newEnv(t)
					raw := mint(t, env, spec)
					p, err := env.Provider.VerifyToken(ctx, raw)
					requerRecusa(t, p, err, raw)
				})
			}

			// Os casos de assinatura. São eles que separam "verifiquei o token"
			// de "li o token".
			assinatura := map[string]TokenSpec{
				"assinado_por_chave_intrusa": {Subject: "s", WrongKey: true},
				"sem_assinatura_alg_none":    {Subject: "s", Unsigned: true},
				"algoritmo_simetrico_hs256":  {Subject: "s", Symmetric: true},
			}
			for nome, spec := range assinatura {
				t.Run(nome, func(t *testing.T) {
					env := newEnv(t)
					if env.Unsigned {
						t.Skipf("ATENÇÃO: este emissor NÃO assina token (%s). "+
							"Verificação de assinatura fica SEM COBERTURA aqui, e a "+
							"cobertura precisa vir de outro ambiente do mesmo adaptador — "+
							"suíte que só rodasse contra ele passaria verde provando nada.",
							env.UnsignedWhy)
					}
					raw := mint(t, env, spec)
					p, err := env.Provider.VerifyToken(ctx, raw)
					requerRecusa(t, p, err, raw)
				})
			}
		})

		t.Run("2_erro_nao_vaza_o_token", func(t *testing.T) {
			env := newEnv(t)
			const segredo = "claim-que-nao-pode-aparecer-em-log"
			raw := mint(t, env, TokenSpec{
				Subject: "s",
				Expiry:  time.Now().Add(-2 * time.Hour),
				Extra:   map[string]any{"segredo_do_token": segredo},
			})
			_, err := env.Provider.VerifyToken(ctx, raw)
			if err == nil {
				t.Fatal("token expirado foi aceito")
			}
			// requerRecusa já compara com as partes do token; aqui o alvo é a
			// claim decodificada, que não trafega em base64 e escaparia da
			// comparação anterior.
			if strings.Contains(err.Error(), segredo) {
				t.Fatalf("VAZAMENTO: a mensagem de erro carrega claim do token: %v", err)
			}
			naoVaza(t, raw, err)
		})

		t.Run("3_sujeito_obrigatorio_e_estavel", func(t *testing.T) {
			env := newEnv(t)
			raw := mint(t, env, TokenSpec{Subject: "sujeito-estavel-42"})
			p, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("token válido recusado: %v", err)
			}
			if p == nil || p.Subject == "" {
				t.Fatal("erro nil com Subject vazio: o domínio ficaria sem identidade nenhuma")
			}
			if p.Subject != "sujeito-estavel-42" {
				t.Fatalf("Subject %q não é o sujeito do token", p.Subject)
			}
			// Garantia 9, a metade determinística: mesmo token, mesmo resultado.
			p2, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("segunda verificação do MESMO token falhou: %v", err)
			}
			if p2.Subject != p.Subject {
				t.Fatalf("o mesmo token deu sujeitos diferentes: %q e %q", p.Subject, p2.Subject)
			}
		})

		t.Run("4_email_nome_e_avatar_sao_opcionais", func(t *testing.T) {
			env := newEnv(t)
			raw := mint(t, env, TokenSpec{Subject: "so-o-sujeito"})
			p, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("token sem e-mail recusado — login por telefone ou SSO sem "+
					"escopo de perfil não teria como entrar: %v", err)
			}
			if p.Email != "" || p.Name != "" || p.AvatarURL != "" {
				t.Fatalf("campos inventados a partir de token que não os traz: %+v", p)
			}
		})

		t.Run("5_email_verified_ausente_e_false", func(t *testing.T) {
			env := newEnv(t)
			raw := mint(t, env, TokenSpec{Subject: "s", Email: "alguem@example.com"})
			p, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("claim ausente virou erro: %v", err)
			}
			if p.EmailVerified {
				t.Fatal("emissor calado promoveu o e-mail a verificado: ausência de " +
					"afirmação não é afirmação")
			}
			sim := true
			env2 := newEnv(t)
			raw2 := mint(t, env2, TokenSpec{Subject: "s", Email: "alguem@example.com", EmailVerified: &sim})
			p2, err := env2.Provider.VerifyToken(ctx, raw2)
			if err != nil {
				t.Fatalf("token com e-mail verificado recusado: %v", err)
			}
			if !p2.EmailVerified {
				t.Fatal("o emissor afirmou e-mail verificado e o adaptador engoliu a afirmação")
			}
		})

		t.Run("6_providers_e_informativo_e_normalizado", func(t *testing.T) {
			env := newEnv(t)
			raw := mint(t, env, TokenSpec{Subject: "s", Providers: []string{"password"}})
			p, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("VerifyToken: %v", err)
			}
			vistos := map[string]bool{}
			for _, v := range p.Providers {
				if v == "" {
					t.Error("entrada vazia em Providers")
				}
				if v != strings.ToLower(v) || v != strings.TrimSpace(v) {
					t.Errorf("provedor fora do vocabulário normalizado: %q", v)
				}
				if vistos[v] {
					t.Errorf("provedor repetido: %q", v)
				}
				vistos[v] = true
			}
			if !vistos["password"] {
				t.Errorf("o token diz que a entrada foi por senha e Providers não reflete: %v", p.Providers)
			}

			env2 := newEnv(t)
			raw2 := mint(t, env2, TokenSpec{Subject: "s"})
			p2, err := env2.Provider.VerifyToken(ctx, raw2)
			if err != nil {
				t.Fatalf("VerifyToken: %v", err)
			}
			if p2.Providers == nil {
				t.Error("Providers nil: 'não sei' é lista VAZIA, e nil acaba " +
					"significando outra coisa na cabeça de quem lê depois")
			}
			if len(p2.Providers) != 0 {
				t.Errorf("adaptador inventou provedor para token que não informa nenhum: %v", p2.Providers)
			}
		})

		t.Run("7_emissor_fora_do_ar_e_unavailable", func(t *testing.T) {
			env := newEnv(t)
			if env.IssuerDown == nil {
				t.Skip("este ambiente não sabe derrubar o emissor — garantia não verificável aqui")
			}
			raw := mint(t, env, TokenSpec{Subject: "s"})
			env.IssuerDown(t) // o cache ainda está frio: a próxima verificação precisa do emissor

			p, err := env.Provider.VerifyToken(ctx, raw)
			if err == nil {
				t.Fatalf("emissor fora do ar e o token foi aceito: %+v", p)
			}
			if k := errs.KindOf(err); k != errs.KindUnavailable {
				t.Fatalf("emissor indisponível virou %s: %v\n"+
					"isso conta ao usuário que a credencial DELE está errada, "+
					"para todo mundo ao mesmo tempo, e manda a equipe procurar o "+
					"defeito no lugar errado", k, err)
			}
			naoVaza(t, raw, err)
		})

		t.Run("7b_contexto_cancelado_e_unavailable", func(t *testing.T) {
			env := newEnv(t)
			if env.Fetches == nil {
				t.Skip("este adaptador não faz I/O para verificar — nada a cancelar")
			}
			raw := mint(t, env, TokenSpec{Subject: "s"})
			cancelado, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := env.Provider.VerifyToken(cancelado, raw)
			if err == nil {
				t.Fatal("contexto cancelado e o token foi verificado assim mesmo")
			}
			if k := errs.KindOf(err); k != errs.KindUnavailable {
				t.Fatalf("contexto cancelado virou %s: %v", k, err)
			}
		})

		t.Run("8_chave_cacheada_e_revalidada_na_rotacao", func(t *testing.T) {
			env := newEnv(t)
			if env.Fetches == nil || env.Rotate == nil {
				t.Skip("este ambiente não observa busca de chave nem sabe rotacionar — " +
					"garantia não verificável aqui")
			}
			raw := mint(t, env, TokenSpec{Subject: "s"})
			if _, err := env.Provider.VerifyToken(ctx, raw); err != nil {
				t.Fatalf("VerifyToken: %v", err)
			}
			apos := env.Fetches()
			if apos == 0 {
				t.Fatal("nenhuma busca de chave: o adaptador não está verificando assinatura")
			}

			// Metade 1: cacheia. Buscar a chave a cada requisição é negação de
			// serviço contra o próprio emissor.
			for i := 0; i < 5; i++ {
				if _, err := env.Provider.VerifyToken(ctx, raw); err != nil {
					t.Fatalf("verificação %d: %v", i, err)
				}
			}
			if n := env.Fetches(); n != apos {
				t.Fatalf("%d buscas de chave para %d verificações: o cache não está segurando", n-apos+1, 6)
			}

			// Metade 2, o freio: `kid` desconhecido logo depois de uma busca é
			// recusado SEM ir ao emissor. Sem isso, quem mandar tokens com kid
			// aleatório usa este processo como gerador de tráfego contra o
			// emissor.
			if intruso, ok := env.Mint(t, TokenSpec{Subject: "s", WrongKey: true}); ok {
				if _, err := env.Provider.VerifyToken(ctx, intruso); errs.KindOf(err) != errs.KindUnauthorized {
					t.Fatalf("token de chave desconhecida deveria ser recusado, veio: %v", err)
				}
				if n := env.Fetches(); n != apos {
					t.Errorf("kid desconhecido disparou busca imediata (%d → %d): "+
						"o freio de rebusca não está no lugar", apos, n)
				}
			}

			// Metade 3: a rotação passa. Sem rebusca, trocar a chave no emissor
			// — rotina no Keycloak e no Firebase — derrubaria todo login.
			env.Rotate(t)
			time.Sleep(env.MinRefresh + 50*time.Millisecond)
			novo := mint(t, env, TokenSpec{Subject: "s"})
			if _, err := env.Provider.VerifyToken(ctx, novo); err != nil {
				t.Fatalf("depois da rotação de chave ninguém mais entra: %v", err)
			}
			if n := env.Fetches(); n <= apos {
				t.Errorf("a chave nova foi aceita sem rebusca (%d → %d) — isso é "+
					"verificação que não está acontecendo", apos, n)
			}
		})

		t.Run("9_seguro_para_uso_concorrente", func(t *testing.T) {
			env := newEnv(t)
			raw := mint(t, env, TokenSpec{Subject: "sujeito-concorrente"})
			const n = 24
			var wg sync.WaitGroup
			erros := make([]error, n)
			subs := make([]string, n)
			wg.Add(n)
			for i := 0; i < n; i++ {
				go func(i int) {
					defer wg.Done()
					p, err := env.Provider.VerifyToken(ctx, raw)
					erros[i] = err
					if p != nil {
						subs[i] = p.Subject
					}
				}(i)
			}
			wg.Wait()
			for i := 0; i < n; i++ {
				if erros[i] != nil {
					t.Fatalf("verificação concorrente %d falhou: %v", i, erros[i])
				}
				if subs[i] != "sujeito-concorrente" {
					t.Fatalf("verificação concorrente %d devolveu sujeito %q", i, subs[i])
				}
			}
		})

		t.Run("10_prefixo_bearer_e_token_vazio", func(t *testing.T) {
			env := newEnv(t)
			raw := mint(t, env, TokenSpec{Subject: "s"})
			com, err := env.Provider.VerifyToken(ctx, "Bearer "+raw)
			if err != nil {
				t.Fatalf("token com o prefixo 'Bearer ' da borda HTTP foi recusado: %v", err)
			}
			sem, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("VerifyToken: %v", err)
			}
			if com.Subject != sem.Subject {
				t.Fatalf("com e sem prefixo deram sujeitos diferentes: %q e %q", com.Subject, sem.Subject)
			}

			vazio := newEnv(t)
			antes := 0
			if vazio.Fetches != nil {
				antes = vazio.Fetches()
			}
			p, err := vazio.Provider.VerifyToken(ctx, "")
			requerRecusa(t, p, err, "")
			if vazio.Fetches != nil && vazio.Fetches() != antes {
				t.Error("token vazio custou uma ida ao emissor: quem chama sem " +
					"credencial não pode virar tráfego contra o provedor de identidade")
			}
		})

		t.Run("11_claim_crua_nao_cruza_a_porta", func(t *testing.T) {
			env := newEnv(t)
			const forjado = "administrador-forjado"
			raw, ok := env.Mint(t, TokenSpec{
				Subject: "s",
				Extra: map[string]any{
					"role":         forjado,
					"custom_claim": forjado,
					"scope":        forjado,
				},
			})
			if !ok {
				t.Skip("este emissor não sabe carregar claim extra — caso não verificável aqui")
			}
			p, err := env.Provider.VerifyToken(ctx, raw)
			if err != nil {
				t.Fatalf("VerifyToken: %v", err)
			}
			campos := append([]string{p.Subject, p.Email, p.Name, p.AvatarURL}, p.Providers...)
			for _, v := range campos {
				if strings.Contains(v, forjado) {
					t.Fatalf("claim de fornecedor atravessou a porta: %q", v)
				}
			}
		})
	})
}

// requerRecusa é a garantia 1 mais a 2, juntas: recusar com o Kind certo e sem
// carregar o token na mensagem.
func requerRecusa(t *testing.T, p *ports.Principal, err error, raw string) {
	t.Helper()
	if err == nil {
		t.Fatalf("token que não se prova foi ACEITO, devolvendo %+v", p)
	}
	if p != nil {
		t.Errorf("erro e Principal ao mesmo tempo: %+v", p)
	}
	if k := errs.KindOf(err); k != errs.KindUnauthorized {
		t.Fatalf("esperava %s, veio %s: %v", errs.KindUnauthorized, k, err)
	}
	naoVaza(t, raw, err)
}

// naoVaza confere que nem o token nem nenhuma das suas partes aparece na
// mensagem. Token em log é credencial em repouso: quem lê o log entra como o
// dono, e é do log que a mensagem de erro vive.
func naoVaza(t *testing.T, raw string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	pedacos := append([]string{raw}, strings.Split(raw, ".")...)
	for _, p := range pedacos {
		// Pedaço curto casa por acaso ("a", ".."); pedaço longo é material do
		// token de verdade.
		if len(p) >= 8 && strings.Contains(msg, p) {
			t.Fatalf("VAZAMENTO: a mensagem de erro carrega o token (ou parte dele): %s", msg)
		}
	}
}
