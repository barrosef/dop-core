//go:build ignore

// Script de publicação dos templates de e-mail no SendGrid. Idempotente.
//
//	SENDGRID_TEMPLATES_API_KEY=SG.xxx \
//	  go run internal/adapter/mailer/publish_templates.go
//
// Ele imprime as linhas de configuração (SENDGRID_TEMPLATE_<TIPO>=d-…) prontas
// para copiar.
//
// É um script, e não um MODO do binário, de propósito: a imagem do dop-core tem
// quatro modos (ADR-0016) e todos são serviços de longa duração. Publicar
// template é operação de manutenção, roda com uma chave DIFERENTE — de
// administração, não de envio — e não deve existir dentro do processo que manda
// e-mail: dar escopo de administração ao worker é dar a ele o poder de
// reescrever o que todo mundo recebe.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/mailer"
)

func main() {
	chave := os.Getenv("SENDGRID_TEMPLATES_API_KEY")
	if chave == "" {
		fmt.Fprintln(os.Stderr, "defina SENDGRID_TEMPLATES_API_KEY (chave de ADMINISTRAÇÃO de templates)")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rs, err := mailer.PublishSendGridTemplates(ctx, mailer.PublishConfig{
		APIKey:  chave,
		BaseURL: os.Getenv("SENDGRID_API"),
	})
	// Imprime o que JÁ deu certo antes de morrer: uma falha no terceiro
	// template não pode fazer perder os ids dos dois primeiros, que já estão
	// criados no fornecedor e precisam ir para a configuração.
	if s := mailer.FormatPublishResults(rs); s != "" {
		fmt.Println(s)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "falha:", err)
		os.Exit(1)
	}
}
