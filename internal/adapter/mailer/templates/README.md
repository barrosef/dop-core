# Templates de e-mail

Dois diretórios, e a duplicação é DELIBERADA — é a consequência negativa que a
ADR-0025 assume por escrito ("dois lugares para o template do mesmo aviso
enquanto os dois adaptadores existirem"). Ela não é acidente nem preguiça: é o
preço de a porta `Mailer` não vazar vocabulário de fornecedor.

| Diretório | Quem usa | Sintaxe | Como chega ao destino |
|---|---|---|---|
| `smtp/` | adaptador SMTP | Go `html/template` (`{{.campo}}`) | embutido no binário (`go:embed`), renderizado no envio |
| `sendgrid/` | adaptador SendGrid | Handlebars (`{{campo}}`) | publicado no fornecedor pelo script idempotente |

As sintaxes são diferentes porque os motores são diferentes — um arquivo só não
serviria aos dois. O que impede um de ficar para trás não é disciplina: é a
suíte de contrato, que exige de TODO adaptador que resolva TODOS os tipos que
`notification.Kinds()` devolve.

## Publicar no SendGrid

O script é idempotente — cria o que falta, cria versão nova do que já existe:

```
SENDGRID_TEMPLATES_API_KEY=SG.xxx \
  go run internal/adapter/mailer/publish_templates.go
```

Ele imprime os `template_id` resultantes, que vão para a configuração
(`SENDGRID_TEMPLATE_<TIPO>`). A lista do que publicar é DERIVADA do índice do
adaptador — não há segunda lista para esquecer de atualizar.
