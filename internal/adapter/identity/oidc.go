// Adaptador de IdentityProvider sobre um emissor OIDC genérico.
//
// É este adaptador que faz a plataforma rodar em cluster self-hosted — Keycloak,
// Dex, Authentik, Zitadel — sem Firebase e sem tocar no domínio, que é o caso de
// portabilidade da ADR-0001. Rodar as MESMAS garantias contra ele e contra o
// Firebase é o que prova que a porta é porta, e não a interface do Firebase com
// outro nome.
//
// Sem SDK de fornecedor: net/http para falar com o emissor e crypto/rsa para a
// assinatura. Não há biblioteca de JOSE no go.mod deste módulo (as únicas
// dependências são gRPC, pgx e NATS), e trazer uma para verificar RS256 seria
// pagar uma árvore de dependências, e uma superfície de CVE, por trinta linhas
// de crypto/rsa. RS256/384/512 é o que emissor OIDC de fato usa.
//
// As primitivas deste arquivo (parseJWT, keySet, validação de claims
// registradas) são do PACOTE, não deste adaptador: o adaptador do Firebase usa
// exatamente as mesmas. Duas implementações de verificação de token no mesmo
// pacote seria duas chances de errar a mesma coisa.
package identity

import (
	"context"
	"crypto"
	"crypto/rsa"
	_ "crypto/sha256" // registra SHA-256/384 para crypto.Hash.New() (RS256/RS384)
	_ "crypto/sha512" // idem para SHA-512 (RS512)
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// defaultClockSkew é a tolerância de relógio aplicada a exp, nbf e iat.
//
// É UM MINUTO, nos dois sentidos, e a escolha é deliberada:
//
//   - sem tolerância nenhuma, qualquer deriva entre o relógio do emissor e o
//     nosso vira "token expirado" intermitente para o usuário. É o pior tipo de
//     falha: aleatória, some quando alguém vai investigar, e culpa a senha de
//     quem digitou certo. Nó com NTP saudável fica em dezenas de milissegundos,
//     mas VM que voltou de suspensão, contêiner recém-agendado e host sem NTP
//     erram segundos;
//   - com tolerância grande, um token roubado continua valendo por todo o tempo
//     tolerado DEPOIS de expirar. Isso é janela de ataque comprada com conforto
//     operacional.
//
// Um minuto é o valor que o OIDC Core sugere para limitar iat, é o que os
// verificadores de referência usam, cobre a deriva realista e mantém a folga
// muito menor que a menor expiração em uso (uma hora, no Firebase).
const defaultClockSkew = time.Minute

// defaultKeysMinRefresh limita quantas vezes um `kid` desconhecido pode mandar
// o processo buscar chave no emissor. Ver keySet.publicKey.
const defaultKeysMinRefresh = 30 * time.Second

// maxIssuerResponse tampa a resposta do emissor. Emissor é infraestrutura de
// terceiro: quando ele adoece, ele não devolve erro — devolve página de proxy,
// ou um fluxo que não termina.
const maxIssuerResponse = 1 << 20 // 1 MiB

func defaultHTTPClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

// ───────────────────────── JWT: análise e verificação ─────────────────────────

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// jwtParts é o token já quebrado, e NADA além disso: análise não é verificação.
// signed é o material exato sobre o qual a assinatura foi feita — as duas
// primeiras partes com o ponto no meio, EM TEXTO, sem re-serializar. Refazer o
// JSON para assinar de novo é como se quebra a verificação sem perceber: um
// espaço a mais e a assinatura legítima não confere.
type jwtParts struct {
	header  jwtHeader
	payload []byte
	signed  []byte
	sig     []byte
}

// parseJWT quebra o token. Toda falha aqui é KindUnauthorized (garantia 1) e
// nenhuma mensagem carrega material do token (garantia 2).
func parseJWT(raw string) (*jwtParts, error) {
	// A borda HTTP entrega "Bearer <token>"; a gRPC entrega o mesmo cabeçalho.
	// Aceitar as duas formas aqui evita que cada chamador invente a sua
	// (garantia 10).
	raw = strings.TrimSpace(raw)
	if len(raw) >= 7 && strings.EqualFold(raw[:7], "bearer ") {
		raw = strings.TrimSpace(raw[7:])
	}
	if raw == "" {
		// Sem I/O: quem chama sem credencial não pode custar uma ida ao emissor.
		return nil, errs.New(errs.KindUnauthorized, "token ausente")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errs.New(errs.KindUnauthorized, "token malformado: esperadas três partes")
	}
	hb, err := decodeSegment(parts[0])
	if err != nil {
		return nil, errs.New(errs.KindUnauthorized, "cabeçalho do token ilegível")
	}
	var h jwtHeader
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, errs.New(errs.KindUnauthorized, "cabeçalho do token não é JSON válido")
	}
	pb, err := decodeSegment(parts[1])
	if err != nil {
		return nil, errs.New(errs.KindUnauthorized, "payload do token ilegível")
	}
	sig, err := decodeSegment(parts[2])
	if err != nil {
		return nil, errs.New(errs.KindUnauthorized, "assinatura do token ilegível")
	}
	return &jwtParts{
		header:  h,
		payload: pb,
		signed:  []byte(parts[0] + "." + parts[1]),
		sig:     sig,
	}, nil
}

// decodeSegment tolera o preenchimento com "=" que alguns emissores mandam,
// embora o JWS o proíba. Recusar token legítimo por causa disso seria rigor sem
// ganho de segurança nenhum.
func decodeSegment(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// checkRSAAlg é a linha de defesa contra confusão de algoritmo.
//
// Aceitamos APENAS RS256/384/512. As duas recusas importantes:
//
//   - "none" dispensa assinatura — é literalmente o que o emulador do Firebase
//     emite, e aceitá-lo aqui significaria que qualquer um monta um token com o
//     `sub` que quiser;
//   - HS* é MAC com segredo compartilhado. Como o "segredo" que teríamos à mão é
//     a chave PÚBLICA do emissor, aceitar HS* deixa o atacante assinar com um
//     dado que ele também tem. É o bug que já derrubou biblioteca famosa de JWT,
//     e ele só existe em verificador que confia no `alg` do próprio token.
func checkRSAAlg(alg string) (crypto.Hash, error) {
	switch alg {
	case "RS256":
		return crypto.SHA256, nil
	case "RS384":
		return crypto.SHA384, nil
	case "RS512":
		return crypto.SHA512, nil
	default:
		// A mensagem não repete o alg recebido: é conteúdo do token.
		return 0, errs.New(errs.KindUnauthorized, "algoritmo de assinatura não aceito")
	}
}

func verifyRSASignature(key *rsa.PublicKey, tok *jwtParts) error {
	h, err := checkRSAAlg(tok.header.Alg)
	if err != nil {
		return err
	}
	sum := h.New()
	sum.Write(tok.signed)
	if err := rsa.VerifyPKCS1v15(key, h, sum.Sum(nil), tok.sig); err != nil {
		return errs.New(errs.KindUnauthorized, "assinatura do token inválida")
	}
	return nil
}

// ───────────────────────── Claims registradas ─────────────────────────

// audience aceita string OU lista, como manda a RFC 7519 §4.1.3. O Firebase
// manda string, o Keycloak manda lista; um verificador que entenda só uma das
// formas recusa token legítimo do outro emissor.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("claim aud em formato inesperado")
	}
	*a = many
	return nil
}

func (a audience) has(v string) bool {
	for _, s := range a {
		if s == v {
			return true
		}
	}
	return false
}

// flexBool existe porque email_verified chega como booleano na maioria dos
// emissores e como a STRING "true" em alguns (Azure AD, realms com mapper de
// atributo). Tratar a string como formato inválido rebaixaria um e-mail de fato
// verificado — e o default inseguro da garantia 5 só é honesto quando o emissor
// realmente não informou nada.
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("claim booleana em formato inesperado")
	}
	switch t := v.(type) {
	case bool:
		*f = flexBool(t)
	case string:
		*f = flexBool(strings.EqualFold(t, "true"))
	case nil:
		*f = false
	default:
		return fmt.Errorf("claim booleana em formato inesperado")
	}
	return nil
}

type registeredClaims struct {
	Iss string   `json:"iss"`
	Sub string   `json:"sub"`
	Aud audience `json:"aud"`
	Exp int64    `json:"exp"`
	Nbf int64    `json:"nbf"`
	Iat int64    `json:"iat"`
}

// validate cobre a garantia 1 na parte que não depende de criptografia. É
// chamada DEPOIS da assinatura, sempre: validar claim de token não assinado é
// perguntar ao atacante se ele é confiável.
func (rc registeredClaims) validate(now time.Time, skew time.Duration, issuer, aud string) error {
	if issuer != "" && rc.Iss != issuer {
		return errs.New(errs.KindUnauthorized, "token de emissor inesperado")
	}
	if aud != "" && !rc.Aud.has(aud) {
		return errs.New(errs.KindUnauthorized, "token emitido para outra audiência")
	}
	// Token sem exp é credencial permanente. Nenhum emissor sério emite um, e
	// aceitar seria deixar o roubo de token virar acesso vitalício.
	if rc.Exp == 0 {
		return errs.New(errs.KindUnauthorized, "token sem expiração")
	}
	if now.After(time.Unix(rc.Exp, 0).Add(skew)) {
		return errs.New(errs.KindUnauthorized, "token expirado")
	}
	if rc.Nbf != 0 && now.Before(time.Unix(rc.Nbf, 0).Add(-skew)) {
		return errs.New(errs.KindUnauthorized, "token ainda não válido")
	}
	if rc.Iat != 0 && now.Before(time.Unix(rc.Iat, 0).Add(-skew)) {
		return errs.New(errs.KindUnauthorized, "token emitido no futuro")
	}
	if rc.Sub == "" {
		return errs.New(errs.KindUnauthorized, "token sem sujeito")
	}
	return nil
}

// ───────────────────────── Cache de chave pública ─────────────────────────

// keySet é o cache de chaves públicas do emissor, e as duas metades da garantia
// 8 vivem aqui.
//
// Buscar a chave a cada requisição é negação de serviço contra o próprio
// emissor: em pico de tráfego, cada login vira uma chamada extra no Keycloak, e
// quando ele cede TODA a plataforma cai junto. Por isso o cache.
//
// Não revalidar, por outro lado, transforma rotação de chave — rotina no
// Keycloak e no Firebase — em queda total de login: os tokens novos vêm com um
// `kid` que o cache não conhece e ninguém entra mais. Por isso o `kid`
// desconhecido dispara uma nova busca.
//
// E é justamente essa busca que precisa de freio: sem ele, quem mandar tokens
// com `kid` aleatório transforma este processo num gerador de tráfego contra o
// emissor — o ataque que o cache existia para evitar, entrando pela porta dos
// fundos. Daí o minRefresh.
type keySet struct {
	mu         sync.Mutex
	keys       map[string]*rsa.PublicKey
	fetchedAt  time.Time
	fetch      func(context.Context) (map[string]*rsa.PublicKey, error)
	minRefresh time.Duration
	now        func() time.Time
}

func (s *keySet) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	// O mutex é segurado DURANTE o I/O de propósito: com mil requisições
	// chegando no instante da rotação, só uma vai ao emissor e as outras
	// esperam por ela. Sem isso, a rotação vira uma estampida contra o emissor.
	s.mu.Lock()
	defer s.mu.Unlock()

	if k, ok := s.lookup(kid); ok {
		return k, nil
	}
	if s.keys != nil && s.now().Sub(s.fetchedAt) < s.minRefresh {
		return nil, errs.New(errs.KindUnauthorized, "token assinado por chave desconhecida")
	}
	keys, err := s.fetch(ctx) // já devolve KindUnavailable (garantia 7)
	if err != nil {
		return nil, err
	}
	s.keys, s.fetchedAt = keys, s.now()
	if k, ok := s.lookup(kid); ok {
		return k, nil
	}
	return nil, errs.New(errs.KindUnauthorized, "token assinado por chave desconhecida")
}

func (s *keySet) lookup(kid string) (*rsa.PublicKey, bool) {
	if len(s.keys) == 0 {
		return nil, false
	}
	if k, ok := s.keys[kid]; ok {
		return k, true
	}
	// `kid` é opcional no JWS. Quando o emissor publica UMA chave só, não há
	// ambiguidade e recusar seria rigor que quebra emissor legítimo. Com duas ou
	// mais, adivinhar seria testar assinatura contra chave qualquer — e aí o
	// `kid` deixaria de ter função.
	if kid == "" && len(s.keys) == 1 {
		for _, k := range s.keys {
			return k, true
		}
	}
	return nil, false
}

// getJSON busca e decodifica JSON do emissor. TODA falha aqui é KindUnavailable,
// nunca KindUnauthorized (garantia 7): o emissor estar fora do ar não diz nada
// sobre o token de quem está chamando.
func getJSON(ctx context.Context, c *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "endereço inválido para o emissor de identidade")
	}
	resp, err := c.Do(req)
	if err != nil {
		// Contexto cancelado ou prazo esgotado também cai aqui, e também é
		// indisponibilidade — não é culpa do token.
		return errs.Wrap(errs.KindUnavailable, err, "emissor de identidade inalcançável")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errs.New(errs.KindUnavailable, "emissor de identidade respondeu HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIssuerResponse))
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "resposta do emissor interrompida")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errs.New(errs.KindUnavailable, "resposta do emissor de identidade ilegível")
	}
	return nil
}

// ───────────────────────── O adaptador ─────────────────────────

type OIDCConfig struct {
	// Issuer é o `iss` EXATO que os tokens carregam, e a base da descoberta.
	Issuer string
	// Audience é o client_id que a plataforma registrou no emissor. Vazio
	// desliga a checagem, e desligar é escolha de quem configura: token válido
	// emitido para OUTRA aplicação do mesmo realm passaria a ser aceito aqui.
	Audience   string
	HTTPClient *http.Client
	// ClockSkew e KeysMinRefresh são AJUSTE do adaptador — a porta não fala
	// disso (ver o bloco "FORA da porta" em ports.IdentityProvider).
	ClockSkew      time.Duration
	KeysMinRefresh time.Duration
	// Now existe para o teste de contrato conseguir provar expiração sem
	// dormir. Em produção é time.Now.
	Now func() time.Time
}

type OIDC struct {
	issuer   string
	audience string
	client   *http.Client
	skew     time.Duration
	now      func() time.Time
	keys     *keySet

	mu      sync.Mutex
	jwksURI string
}

func NewOIDC(cfg OIDCConfig) *OIDC {
	o := &OIDC{
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		client:   cfg.HTTPClient,
		skew:     cfg.ClockSkew,
		now:      cfg.Now,
	}
	if o.client == nil {
		o.client = defaultHTTPClient()
	}
	if o.skew == 0 {
		o.skew = defaultClockSkew
	}
	if o.now == nil {
		o.now = time.Now
	}
	minRefresh := cfg.KeysMinRefresh
	if minRefresh == 0 {
		minRefresh = defaultKeysMinRefresh
	}
	o.keys = &keySet{fetch: o.fetchJWKS, minRefresh: minRefresh, now: o.now}
	return o
}

// jwksEndpoint resolve o jwks_uri por descoberta, uma vez.
//
// A descoberta é PREGUIÇOSA de propósito: fazê-la no boot amarraria a subida do
// processo à do Keycloak, e num cluster que sobe tudo junto isso é um
// CrashLoopBackOff garantido no primeiro deploy. Falhou, tenta de novo na
// próxima verificação — o custo é uma requisição, não o processo.
func (o *OIDC) jwksEndpoint(ctx context.Context) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.jwksURI != "" {
		return o.jwksURI, nil
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	url := strings.TrimRight(o.issuer, "/") + "/.well-known/openid-configuration"
	if err := getJSON(ctx, o.client, url, &doc); err != nil {
		return "", err
	}
	// O documento tem de declarar o MESMO emissor que configuramos. Sem esta
	// checagem, quem controlar o DNS ou o roteamento aponta a descoberta para
	// outro emissor e passa a assinar os nossos tokens. É indisponibilidade, e
	// não token inválido: o emissor confiável é que não está lá.
	if !sameIssuer(doc.Issuer, o.issuer) {
		return "", errs.New(errs.KindUnavailable,
			"descoberta declara emissor %q, esperado %q", doc.Issuer, o.issuer)
	}
	if doc.JWKSURI == "" {
		return "", errs.New(errs.KindUnavailable, "descoberta sem jwks_uri")
	}
	o.jwksURI = doc.JWKSURI
	return o.jwksURI, nil
}

// sameIssuer compara ignorando a barra final. Alguns emissores publicam o `iss`
// com barra (Auth0) e outros sem; a diferença não é semântica, mas derruba a
// comparação exata de quem configurou de um jeito e recebeu do outro.
func sameIssuer(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (o *OIDC) fetchJWKS(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	uri, err := o.jwksEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := getJSON(ctx, o.client, uri, &doc); err != nil {
		return nil, err
	}
	out := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		// Chave de cifragem não assina, e chave que não é RSA este adaptador não
		// verifica: ignorar em silêncio é o certo aqui, porque o JWKS de um
		// realm real tem material que não é para nós.
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		pub, err := rsaFromJWK(k)
		if err != nil {
			continue
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, errs.New(errs.KindUnavailable, "emissor não publicou chave RSA de assinatura utilizável")
	}
	return out, nil
}

func rsaFromJWK(k jwk) (*rsa.PublicKey, error) {
	nb, err := decodeSegment(k.N)
	if err != nil {
		return nil, err
	}
	eb, err := decodeSegment(k.E)
	if err != nil {
		return nil, err
	}
	e := new(big.Int).SetBytes(eb)
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31 {
		return nil, fmt.Errorf("expoente fora da faixa aceitável")
	}
	n := new(big.Int).SetBytes(nb)
	// Módulo curto é chave fraca: 2048 bits é o mínimo que qualquer emissor de
	// produção usa, e aceitar menos seria aceitar assinatura que dá para forjar.
	if n.BitLen() < 2048 {
		return nil, fmt.Errorf("módulo RSA curto demais")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

type oidcClaims struct {
	registeredClaims
	Email             string   `json:"email"`
	EmailVerified     flexBool `json:"email_verified"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	Picture           string   `json:"picture"`
	// Amr é como o sujeito se autenticou; idp/identity_provider é o provedor
	// externo intermediado (broker do Keycloak). Nenhum dos dois é obrigatório —
	// e é por isso que a garantia 6 permite lista vazia.
	Amr              []string `json:"amr"`
	Idp              string   `json:"idp"`
	IdentityProvider string   `json:"identity_provider"`
}

func (o *OIDC) VerifyToken(ctx context.Context, raw string) (*ports.Principal, error) {
	tok, err := parseJWT(raw)
	if err != nil {
		return nil, err
	}
	if _, err := checkRSAAlg(tok.header.Alg); err != nil {
		return nil, err
	}
	key, err := o.keys.publicKey(ctx, tok.header.Kid)
	if err != nil {
		return nil, err
	}
	if err := verifyRSASignature(key, tok); err != nil {
		return nil, err
	}
	// Só agora as claims valem alguma coisa: a assinatura confere.
	var c oidcClaims
	if err := json.Unmarshal(tok.payload, &c); err != nil {
		return nil, errs.New(errs.KindUnauthorized, "claims do token ilegíveis")
	}
	if err := c.validate(o.now(), o.skew, o.issuer, o.audience); err != nil {
		return nil, err
	}
	return &ports.Principal{
		Subject:       c.Sub,
		Email:         c.Email,
		EmailVerified: bool(c.EmailVerified),
		// preferred_username é o que o Keycloak preenche sempre; `name` só
		// aparece com o escopo de perfil. Cair de um para o outro dá ao domínio
		// um rótulo utilizável em vez de vazio — e continua sendo campo
		// OPCIONAL, que ninguém pode usar como identidade (garantia 4).
		Name:      firstNonEmpty(c.Name, c.PreferredUsername),
		AvatarURL: c.Picture,
		Providers: normalizeProviders(append([]string{c.Idp, c.IdentityProvider}, c.Amr...)),
	}, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// normalizeProviders devolve minúsculas, sem vazio e sem repetição, e SEMPRE uma
// fatia não-nula: a garantia 6 diz que "não sei" é lista VAZIA, e nil que
// significa alguma coisa é a próxima interpretação errada esperando acontecer.
func normalizeProviders(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		// "pwd" é o nome que o OIDC dá para o que o Firebase chama de
		// "password". Normalizar aqui é o que torna Providers comparável entre
		// os dois adaptadores — sem isso, o campo seria vocabulário do emissor
		// vazando pela porta com outro nome.
		if v == "pwd" {
			v = "password"
		}
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

var _ ports.IdentityProvider = (*OIDC)(nil)
