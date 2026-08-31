// Package idem implementa a idempotência exigida em toda RPC de escrita
// (ADR-0017): com eventos e retries, repetir uma chamada não pode duplicar efeito.
package idem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Store guarda o resultado de uma operação por chave, para que a repetição
// devolva a mesma resposta em vez de executar de novo.
type Store interface {
	// Begin tenta reservar a chave. Se já existir com o mesmo hash de
	// requisição, devolve a resposta gravada e done=true.
	Begin(ctx context.Context, key, requestHash string, ttl time.Duration) (response []byte, done bool, err error)
	// Complete grava a resposta da execução bem-sucedida.
	Complete(ctx context.Context, key string, response []byte) error
}

// Hash gera a assinatura da requisição — a mesma chave com corpo diferente é
// conflito, não repetição.
func Hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

const DefaultTTL = 24 * time.Hour
