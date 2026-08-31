//go:build integration

// A MESMA suíte de contrato, agora contra o Storage emulado do Firebase.
//
//	go test ./test/contract/ -tags=integration -v
//
// O adaptador de arquivos e o de GCS têm implementações sem nada em comum;
// é só passando os dois pela mesma suíte que "trocar de adaptador não muda o
// comportamento" deixa de ser promessa e vira fato verificado.
package contract_test

import (
	"os"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/adapter/objectstore"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// DOIS DEFEITOS CONHECIDOS DO EMULADOR — subtestes 8 e 13.
//
// Com uploadType=media e Content-Type EXATAMENTE "application/json", o emulador
// de Storage do Firebase nunca responde: a conexão fica pendurada até o timeout
// do cliente. Reproduzível fora do teste, com curl e o mesmo corpo:
//
//	application/json          → pendura (sem resposta)
//	application/json; charset=utf-8 → 400
//	text/plain, text/json, application/xml, application/octet-stream → 200
//
// O GCS de verdade aceita e guarda normalmente. É diferença do emulador, não do
// adaptador — provado pelo `fs`, que passa nos 13 subtestes.
//
// Consequência prática: guardar JSON no object store PENDURA no ambiente local.
// Enquanto o emulador não corrigir, quem gravar JSON deve usar um tipo que ele
// aceite. Documentado em dop-infra/docs/ambiente-local.md; o alvo
// `test-contract-integration` do Makefile exclui este subteste, com o motivo à
// vista — em vez de deixá-lo vermelho para sempre e todo mundo aprender a
// ignorar a suíte.
func TestObjectStoreContractGCS(t *testing.T) {
	host := os.Getenv("STORAGE_EMULATOR_HOST")
	if host == "" {
		host = "127.0.0.1:9199"
	}
	bucket := os.Getenv("STORAGE_BUCKET")
	if bucket == "" {
		bucket = "dop-local.firebasestorage.app"
	}
	contract.ObjectStoreSuite(t, "gcs-emulado", func(t *testing.T) (ports.ObjectStore, []string) {
		// Um balde só: o emulador não provisiona um segundo, e inventar um
		// faria o caso de isolamento falhar por motivo errado.
		return objectstore.NewGCS(objectstore.GCSConfig{Endpoint: host}), []string{bucket}
	})
}
