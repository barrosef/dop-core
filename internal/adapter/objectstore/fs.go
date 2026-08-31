// Adaptador de ObjectStore sobre o sistema de arquivos local.
//
// Por que ele é adaptador de primeira classe e não brinquedo de teste: é o que
// faz a instalação self-hosted rodar sem GCS nenhum — um volume no cluster
// basta. É também o par honesto do gcs.go: dois adaptadores de tecnologias sem
// nenhum parentesco passando o MESMO contrato é o que prova que a porta é
// abstração e não fachada do SDK do Google (ADR-0001).
//
// A chave é OPACA e PLANA, como no GCS: "a/b" é um NOME que por acaso tem uma
// barra, não um caminho. Por isso o nome vai escapado para o disco em vez de
// virar diretório — três consequências que a suíte de contrato cobra:
//   - "a/b" e "a/b/c" coexistem (com árvore de diretórios, um seria pasta e o
//     outro arquivo, e a segunda escrita falharia);
//   - "../fora" é uma chave literal DENTRO do bucket, não uma fuga;
//   - chave longa não estoura o limite de 255 bytes de nome de arquivo.
package objectstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"context"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// FS guarda cada bucket em um diretório sob root, com conteúdo e metadado em
// subárvores irmãs — nunca no mesmo diretório, senão o metadado de uma chave
// poderia colidir com o conteúdo de outra chamada "x.meta".
type FS struct{ root string }

const (
	dirObjetos   = "obj"
	dirMetadados = "meta"

	// 255 é o limite de nome de arquivo na maioria dos sistemas; o resto do
	// orçamento fica para o sufixo de hash.
	maxNomeCodificado = 200

	permDir  fs.FileMode = 0o700
	permFile fs.FileMode = 0o600
)

// NewFS prepara a raiz. Falha aqui, no boot, é melhor que falha no primeiro
// upload — que é sempre longe da causa.
func NewFS(root string) (*FS, error) {
	if root == "" {
		return nil, errs.Invalid("raiz do armazenamento em arquivo não informada")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, errs.Wrap(errs.KindInvalid, err, "raiz inválida: %q", root)
	}
	if err := os.MkdirAll(abs, permDir); err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, err, "não foi possível criar a raiz %q", abs)
	}
	return &FS{root: abs}, nil
}

// codifica transforma um nome opaco em nome de arquivo seguro e ÚNICO.
//
// url.PathEscape resolve barra, porcento e espaço, mas não resolve "." e ".."
// (que não precisam de escape numa URL e são veneno num caminho) nem o limite
// de tamanho. Nesses dois casos o hash entra e a injetividade continua valendo:
// dois nomes diferentes nunca colidem no disco.
func codifica(nome string) string {
	e := url.PathEscape(nome)
	if e != "." && e != ".." && len(e) <= maxNomeCodificado {
		return e
	}
	soma := sha256.Sum256([]byte(nome))
	prefixo := e
	if len(prefixo) > maxNomeCodificado-40 {
		prefixo = prefixo[:maxNomeCodificado-40]
	}
	return prefixo + "~" + hex.EncodeToString(soma[:])[:32]
}

func (f *FS) caminhos(ref ports.ObjectRef) (obj, meta string, err error) {
	if ref.Bucket == "" {
		return "", "", errs.Invalid("bucket não informado")
	}
	if ref.Key == "" {
		return "", "", errs.Invalid("chave do objeto não informada")
	}
	b := codifica(ref.Bucket)
	k := codifica(ref.Key)
	return filepath.Join(f.root, b, dirObjetos, k),
		filepath.Join(f.root, b, dirMetadados, k), nil
}

type metadado struct {
	ContentType string `json:"content_type"`
}

// escreveAtomico grava por arquivo temporário + rename.
//
// rename dentro do mesmo diretório é atômico no POSIX: um leitor concorrente vê
// a versão antiga OU a nova, jamais meio objeto. A porta promete substituição
// atômica e é aqui que ela é cumprida — escrever direto no destino truncaria o
// arquivo à vista de quem estivesse lendo.
func escreveAtomico(destino string, conteudo []byte) error {
	dir := filepath.Dir(destino)
	if err := os.MkdirAll(dir, permDir); err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "não foi possível preparar %q", dir)
	}
	tmp, err := os.CreateTemp(dir, ".parcial-*")
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "falha ao abrir arquivo temporário")
	}
	nome := tmp.Name()
	defer os.Remove(nome) // inócuo quando o rename já levou o arquivo embora

	if _, err := tmp.Write(conteudo); err != nil {
		tmp.Close()
		return errs.Wrap(errs.KindUnavailable, err, "falha ao gravar objeto")
	}
	if err := tmp.Chmod(permFile); err != nil {
		tmp.Close()
		return errs.Wrap(errs.KindUnavailable, err, "falha ao ajustar permissão do objeto")
	}
	if err := tmp.Close(); err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "falha ao fechar objeto")
	}
	if err := os.Rename(nome, destino); err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "falha ao publicar objeto")
	}
	return nil
}

func (f *FS) Put(_ context.Context, ref ports.ObjectRef, content []byte, contentType string) error {
	obj, meta, err := f.caminhos(ref)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream" // mesma escolha do gcs.go
	}
	if err := escreveAtomico(obj, content); err != nil {
		return err
	}
	m, _ := json.Marshal(metadado{ContentType: contentType})
	return escreveAtomico(meta, m)
}

func (f *FS) Get(_ context.Context, ref ports.ObjectRef) ([]byte, error) {
	obj, _, err := f.caminhos(ref)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(obj)
	if err != nil {
		if ausente(err) {
			return nil, errs.NotFound("objeto %s/%s", ref.Bucket, ref.Key)
		}
		return nil, errs.Wrap(errs.KindUnavailable, err, "falha ao ler objeto")
	}
	return b, nil
}

func (f *FS) Delete(_ context.Context, ref ports.ObjectRef) error {
	obj, meta, err := f.caminhos(ref)
	if err != nil {
		return err
	}
	// Idempotente: remover o que não existe é sucesso, como no GCS.
	for _, p := range []string{obj, meta} {
		if err := os.Remove(p); err != nil && !ausente(err) {
			return errs.Wrap(errs.KindUnavailable, err, "falha ao remover objeto")
		}
	}
	return nil
}

func (f *FS) Stat(_ context.Context, ref ports.ObjectRef) (*ports.ObjectMeta, error) {
	obj, meta, err := f.caminhos(ref)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(obj)
	if err != nil || fi.IsDir() {
		if err == nil || ausente(err) {
			return nil, errs.NotFound("objeto %s/%s", ref.Bucket, ref.Key)
		}
		return nil, errs.Wrap(errs.KindUnavailable, err, "falha ao consultar objeto")
	}
	out := &ports.ObjectMeta{
		Size:      fi.Size(),
		UpdatedAt: fi.ModTime().UTC(),
		// Metadado perdido (crash entre as duas escritas) não invalida o
		// objeto: cai no mesmo padrão que o Put usa quando ninguém informa tipo.
		ContentType: "application/octet-stream",
	}
	if b, err := os.ReadFile(meta); err == nil {
		var m metadado
		if json.Unmarshal(b, &m) == nil && m.ContentType != "" {
			out.ContentType = m.ContentType
		}
	}
	return out, nil
}

// SignedPutURL e SignedGetURL não existem aqui, e o silêncio seria pior.
//
// URL assinada é upload que não passa pelo BFF; com armazenamento em arquivo
// não há endpoint para assinar. Devolver "file:///..." seria mentir para o
// chamador, que entregaria ao navegador uma URL que ninguém consegue usar.
// KindUnavailable é a resposta honesta e faz o chamador cair no upload via BFF
// — que é o caminho correto no self-hosted. A porta documenta essa alternativa
// e a suíte de contrato a cobra dos dois adaptadores.
func (f *FS) SignedPutURL(context.Context, ports.ObjectRef, time.Duration) (string, error) {
	return "", errs.New(errs.KindUnavailable,
		"armazenamento em arquivo não emite URL assinada; o upload passa pelo BFF")
}

func (f *FS) SignedGetURL(context.Context, ports.ObjectRef, time.Duration) (string, error) {
	return "", errs.New(errs.KindUnavailable,
		"armazenamento em arquivo não emite URL assinada; a leitura passa pelo BFF")
}

func ausente(err error) bool { return errors.Is(err, fs.ErrNotExist) }

var _ ports.ObjectStore = (*FS)(nil)
