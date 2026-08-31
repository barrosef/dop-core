package contract

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ObjectStoreSuite verifica as nove garantias documentadas na porta.
//
// newStore devolve o adaptador e os buckets utilizáveis naquele ambiente. O
// primeiro serve a todos os casos; o segundo, quando existe, exercita o
// isolamento entre buckets — no GCS emulado nem sempre há dois buckets
// provisionados, e inventar um faria o teste falhar por motivo errado.
func ObjectStoreSuite(t *testing.T, name string, newStore func(t *testing.T) (ports.ObjectStore, []string)) {
	t.Run(name, func(t *testing.T) {
		ctx := context.Background()

		// Prefixo único por execução: o bucket pode ser compartilhado entre
		// rodadas (e entre adaptadores) e chave de teste não pode se cruzar.
		prefixo := func(t *testing.T) string {
			return fmt.Sprintf("contrato/%d-%d/", time.Now().UnixNano(), chaveSeq.Add(1))
		}

		t.Run("1_leitura_apos_escrita_imediata", func(t *testing.T) {
			s, buckets := newStore(t)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: prefixo(t) + "diagrama.svg"}
			conteudo := []byte("<svg>desenho</svg>")

			if err := s.Put(ctx, ref, conteudo, "image/svg+xml"); err != nil {
				t.Fatalf("Put: %v", err)
			}
			t.Cleanup(func() { _ = s.Delete(context.Background(), ref) })

			got, err := s.Get(ctx, ref)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !bytes.Equal(got, conteudo) {
				t.Fatalf("conteúdo divergente: %q != %q", got, conteudo)
			}
		})

		t.Run("2_put_substitui_o_objeto_inteiro", func(t *testing.T) {
			s, buckets := newStore(t)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: prefixo(t) + "nota.txt"}
			t.Cleanup(func() { _ = s.Delete(context.Background(), ref) })

			if err := s.Put(ctx, ref, []byte("versão antiga, bem mais comprida"), "text/plain"); err != nil {
				t.Fatalf("1º Put: %v", err)
			}
			if err := s.Put(ctx, ref, []byte("nova"), "text/plain"); err != nil {
				t.Fatalf("2º Put: %v", err)
			}
			got, err := s.Get(ctx, ref)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			// Substituição é total: sobra de bytes antigos significa escrita
			// por cima sem truncar, que é corrupção silenciosa.
			if string(got) != "nova" {
				t.Fatalf("esperava 'nova', veio %q", got)
			}
			meta, err := s.Stat(ctx, ref)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if meta.Size != 4 {
				t.Fatalf("Stat.Size = %d, esperava 4 — o tamanho ficou na versão antiga", meta.Size)
			}
		})

		t.Run("3_ausencia_e_erro_not_found", func(t *testing.T) {
			s, buckets := newStore(t)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: prefixo(t) + "nunca-gravado"}

			_, err := s.Get(ctx, ref)
			if err == nil {
				t.Fatal("Get de objeto ausente deveria falhar (aqui a ausência é erro, ao contrário do SecretStore)")
			}
			if k := errs.KindOf(err); k != errs.KindNotFound {
				t.Fatalf("Get de ausente devolveu kind %q, esperava %q", k, errs.KindNotFound)
			}
			if _, err := s.Stat(ctx, ref); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("Stat de ausente devolveu %v, esperava kind %q", err, errs.KindNotFound)
			}
		})

		t.Run("4_delete_idempotente", func(t *testing.T) {
			s, buckets := newStore(t)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: prefixo(t) + "temporario"}

			if err := s.Put(ctx, ref, []byte("x"), ""); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if err := s.Delete(ctx, ref); err != nil {
				t.Fatalf("1º Delete: %v", err)
			}
			if err := s.Delete(ctx, ref); err != nil {
				t.Fatalf("2º Delete deveria ser inócuo: %v", err)
			}
			if _, err := s.Get(ctx, ref); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("o objeto sobreviveu ao Delete: %v", err)
			}
		})

		t.Run("5_chave_e_opaca_e_plana", func(t *testing.T) {
			s, buckets := newStore(t)
			p := prefixo(t)
			pai := ports.ObjectRef{Bucket: buckets[0], Key: p + "a/b"}
			filho := ports.ObjectRef{Bucket: buckets[0], Key: p + "a/b/c"}
			t.Cleanup(func() {
				_ = s.Delete(context.Background(), pai)
				_ = s.Delete(context.Background(), filho)
			})

			// Ordem proposital: primeiro o "filho", depois o "pai". Um adaptador
			// que tratasse a barra como diretório falharia aqui — "a/b" já seria
			// pasta. A barra é caractere do NOME, não hierarquia.
			if err := s.Put(ctx, filho, []byte("do filho"), "text/plain"); err != nil {
				t.Fatalf("Put do filho: %v", err)
			}
			if err := s.Put(ctx, pai, []byte("do pai"), "text/plain"); err != nil {
				t.Fatalf("Put de 'a/b' com 'a/b/c' existente: %v", err)
			}
			for ref, esperado := range map[ports.ObjectRef]string{pai: "do pai", filho: "do filho"} {
				got, err := s.Get(ctx, ref)
				if err != nil {
					t.Fatalf("Get %q: %v", ref.Key, err)
				}
				if string(got) != esperado {
					t.Fatalf("Get %q devolveu %q, esperava %q", ref.Key, got, esperado)
				}
			}
			// Prefixo não é objeto.
			if _, err := s.Get(ctx, ports.ObjectRef{Bucket: buckets[0], Key: p + "a"}); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("prefixo 'a' não é objeto e deveria dar not_found, veio %v", err)
			}
		})

		t.Run("6_chave_nao_escapa_do_bucket", func(t *testing.T) {
			s, buckets := newStore(t)
			p := prefixo(t)
			fuga := ports.ObjectRef{Bucket: buckets[0], Key: p + "../" + p + "fuga"}
			alvo := ports.ObjectRef{Bucket: buckets[0], Key: p + "fuga"}
			t.Cleanup(func() { _ = s.Delete(context.Background(), fuga) })

			// Duas respostas são aceitáveis — recusar a chave, ou tratá-la como
			// NOME literal. O que não é aceitável é gravar em outro lugar.
			if err := s.Put(ctx, fuga, []byte("conteúdo fugitivo"), ""); err != nil {
				return
			}
			if _, err := s.Get(ctx, alvo); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("a chave com '..' escapou: %q passou a existir (%v)", alvo.Key, err)
			}
		})

		t.Run("7_isolamento_entre_buckets", func(t *testing.T) {
			s, buckets := newStore(t)
			if len(buckets) < 2 {
				t.Skip("ambiente com um bucket só — isolamento entre buckets não é exercitável aqui")
			}
			chave := prefixo(t) + "mesmo-nome"
			a := ports.ObjectRef{Bucket: buckets[0], Key: chave}
			b := ports.ObjectRef{Bucket: buckets[1], Key: chave}
			t.Cleanup(func() {
				_ = s.Delete(context.Background(), a)
				_ = s.Delete(context.Background(), b)
			})

			if err := s.Put(ctx, a, []byte("do bucket A"), ""); err != nil {
				t.Fatalf("Put em A: %v", err)
			}
			if _, err := s.Get(ctx, b); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("VAZAMENTO ENTRE BUCKETS: a mesma chave resolveu em B (%v)", err)
			}
		})

		t.Run("8_metadados", func(t *testing.T) {
			s, buckets := newStore(t)
			p := prefixo(t)
			comTipo := ports.ObjectRef{Bucket: buckets[0], Key: p + "com-tipo.json"}
			semTipo := ports.ObjectRef{Bucket: buckets[0], Key: p + "sem-tipo.bin"}
			t.Cleanup(func() {
				_ = s.Delete(context.Background(), comTipo)
				_ = s.Delete(context.Background(), semTipo)
			})

			conteudo := []byte(`{"a":1}`)
			if err := s.Put(ctx, comTipo, conteudo, "application/json"); err != nil {
				t.Fatalf("Put: %v", err)
			}
			meta, err := s.Stat(ctx, comTipo)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if meta.Size != int64(len(conteudo)) {
				t.Errorf("Stat.Size = %d, esperava %d", meta.Size, len(conteudo))
			}
			if meta.ContentType != "application/json" {
				t.Errorf("Stat.ContentType = %q, esperava application/json", meta.ContentType)
			}
			if meta.UpdatedAt.IsZero() {
				t.Error("Stat.UpdatedAt zerado: sem ele não há como saber qual versão está lá")
			}

			// Put sem tipo cai no padrão binário — mesma escolha nos dois adaptadores.
			if err := s.Put(ctx, semTipo, []byte("bytes"), ""); err != nil {
				t.Fatalf("Put sem tipo: %v", err)
			}
			m2, err := s.Stat(ctx, semTipo)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if m2.ContentType != "application/octet-stream" {
				t.Errorf("sem Content-Type o padrão deveria ser application/octet-stream, veio %q", m2.ContentType)
			}

			// UpdatedAt não regride na reescrita.
			antes := meta.UpdatedAt
			time.Sleep(5 * time.Millisecond)
			if err := s.Put(ctx, comTipo, []byte(`{"a":2}`), "application/json"); err != nil {
				t.Fatalf("reescrita: %v", err)
			}
			depois, err := s.Stat(ctx, comTipo)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if depois.UpdatedAt.Before(antes) {
				t.Errorf("UpdatedAt regrediu: %v depois de %v", depois.UpdatedAt, antes)
			}
		})

		t.Run("9_objeto_vazio_e_valido", func(t *testing.T) {
			s, buckets := newStore(t)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: prefixo(t) + "vazio"}
			t.Cleanup(func() { _ = s.Delete(context.Background(), ref) })

			// Upload de arquivo vazio existe e não pode virar "não existe".
			if err := s.Put(ctx, ref, nil, "text/plain"); err != nil {
				t.Fatalf("Put vazio: %v", err)
			}
			got, err := s.Get(ctx, ref)
			if err != nil {
				t.Fatalf("Get do objeto vazio: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("esperava zero bytes, vieram %d", len(got))
			}
			meta, err := s.Stat(ctx, ref)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if meta.Size != 0 {
				t.Fatalf("Stat.Size = %d para objeto vazio", meta.Size)
			}
		})

		t.Run("10_chave_longa", func(t *testing.T) {
			s, buckets := newStore(t)
			// 300 caracteres: acima do limite de nome de arquivo (255) e abaixo
			// do limite de chave do GCS (1024). Adaptador de disco que mapeia
			// chave para nome de arquivo cru quebra exatamente aqui.
			longa := prefixo(t) + strings.Repeat("k", 300)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: longa}
			t.Cleanup(func() { _ = s.Delete(context.Background(), ref) })

			if err := s.Put(ctx, ref, []byte("chave comprida"), ""); err != nil {
				t.Fatalf("Put com chave de %d bytes: %v", len(longa), err)
			}
			got, err := s.Get(ctx, ref)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if string(got) != "chave comprida" {
				t.Fatalf("conteúdo divergente: %q", got)
			}
			// Duas chaves longas diferentes não podem colidir no destino.
			outra := ports.ObjectRef{Bucket: buckets[0], Key: longa + "-outra"}
			t.Cleanup(func() { _ = s.Delete(context.Background(), outra) })
			if err := s.Put(ctx, outra, []byte("outro objeto"), ""); err != nil {
				t.Fatalf("Put da segunda chave longa: %v", err)
			}
			if got, _ := s.Get(ctx, ref); string(got) != "chave comprida" {
				t.Fatalf("COLISÃO: a segunda chave longa sobrescreveu a primeira (%q)", got)
			}
		})

		t.Run("11_chave_vazia_e_recusada", func(t *testing.T) {
			s, buckets := newStore(t)
			if err := s.Put(ctx, ports.ObjectRef{Bucket: buckets[0], Key: ""}, []byte("x"), ""); err == nil {
				t.Fatal("objeto sem chave não deveria ser aceito")
			}
		})

		t.Run("12_url_assinada_ou_indisponivel_explicito", func(t *testing.T) {
			s, buckets := newStore(t)
			ref := ports.ObjectRef{Bucket: buckets[0], Key: prefixo(t) + "upload-direto"}

			for nome, fn := range map[string]func(context.Context, ports.ObjectRef, time.Duration) (string, error){
				"SignedPutURL": s.SignedPutURL,
				"SignedGetURL": s.SignedGetURL,
			} {
				// A URL vale para objeto que ainda não existe — é assim que o
				// upload direto funciona: assina antes, o cliente grava depois.
				u, err := fn(ctx, ref, 10*time.Minute)
				switch {
				case err == nil && u == "":
					t.Errorf("%s devolveu string vazia SEM erro — o chamador não tem como saber "+
						"que precisa cair no upload pelo BFF", nome)
				case err != nil && errs.KindOf(err) != errs.KindUnavailable:
					t.Errorf("%s falhou com kind %q; a porta admite só %q para capacidade ausente",
						nome, errs.KindOf(err), errs.KindUnavailable)
				}
			}
		})

		t.Run("13_uso_concorrente", func(t *testing.T) {
			s, buckets := newStore(t)
			p := prefixo(t)

			// Escritas em chaves distintas não podem se atrapalhar.
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					ref := ports.ObjectRef{Bucket: buckets[0], Key: fmt.Sprintf("%sconc-%d", p, i)}
					if err := s.Put(ctx, ref, []byte(fmt.Sprintf("objeto %d", i)), ""); err != nil {
						t.Errorf("Put concorrente %d: %v", i, err)
					}
				}(i)
			}
			wg.Wait()
			for i := 0; i < 16; i++ {
				ref := ports.ObjectRef{Bucket: buckets[0], Key: fmt.Sprintf("%sconc-%d", p, i)}
				got, err := s.Get(ctx, ref)
				if err != nil {
					t.Fatalf("Get concorrente %d: %v", i, err)
				}
				if string(got) != fmt.Sprintf("objeto %d", i) {
					t.Fatalf("objeto %d embaralhado: %q", i, got)
				}
				_ = s.Delete(ctx, ref)
			}

			// Substituição é atômica para quem lê: ou a versão curta, ou a
			// longa — nunca um pedaço das duas.
			ref := ports.ObjectRef{Bucket: buckets[0], Key: p + "disputado"}
			t.Cleanup(func() { _ = s.Delete(context.Background(), ref) })
			curta := []byte("curta")
			longa := bytes.Repeat([]byte("L"), 4096)
			if err := s.Put(ctx, ref, curta, ""); err != nil {
				t.Fatalf("Put inicial: %v", err)
			}
			parar := make(chan struct{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-parar:
						return
					default:
					}
					v := curta
					if i%2 == 0 {
						v = longa
					}
					if err := s.Put(ctx, ref, v, ""); err != nil {
						t.Errorf("Put durante leitura: %v", err)
						return
					}
				}
			}()
			for i := 0; i < 50; i++ {
				got, err := s.Get(ctx, ref)
				if err != nil {
					close(parar)
					wg.Wait()
					t.Fatalf("Get durante escrita: %v", err)
				}
				if !bytes.Equal(got, curta) && !bytes.Equal(got, longa) {
					close(parar)
					wg.Wait()
					t.Fatalf("LEITURA PARCIAL: %d bytes que não são nenhuma das duas versões", len(got))
				}
			}
			close(parar)
			wg.Wait()
		})
	})
}

// chaveSeq garante prefixo distinto mesmo dentro do mesmo nanossegundo.
var chaveSeq atomic.Int64
