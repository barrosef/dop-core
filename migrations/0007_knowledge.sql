-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- Conhecimento: as três camadas da ADR-0009 em UMA tabela.
--
-- Regra, índice e memória são a mesma coisa do ponto de vista do
-- armazenamento — texto versionado, com escopo e dono — e diferem no CICLO
-- (quem escreve, quando, e como é buscado). Três tabelas obrigariam a repetir
-- escopo, versão, isolamento por conta e o par body/object_ref três vezes, e a
-- montagem do pacote — que lê as três — viraria três consultas com o mesmo
-- WHERE. O tipo é coluna; o que muda por camada é o ÍNDICE, e é lá embaixo que
-- a diferença aparece.
-- ════════════════════════════════════════════════════════════════════════════

CREATE TYPE knowledge_kind  AS ENUM ('rule', 'index', 'memory');
CREATE TYPE knowledge_scope AS ENUM ('account', 'workspace', 'project');

CREATE TABLE knowledge_artifacts (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,

  -- Escopo e herança: uma regra da conta vale para todo projeto dela, e um
  -- projeto pode substituí-la por regra de MESMO NOME. Sem os três níveis, a
  -- alternativa seria copiar a política da casa em cada projeto — que é como
  -- base de conhecimento apodrece.
  scope        knowledge_scope NOT NULL,
  workspace_id uuid REFERENCES workspaces(id) ON DELETE CASCADE,
  project_id   uuid REFERENCES projects(id) ON DELETE CASCADE,
  -- Identidade do escopo em UMA coluna. Existe para que a unicidade e o
  -- ON CONFLICT do upsert não dependam de um COALESCE repetido em toda query —
  -- expressão duplicada é expressão que uma hora diverge.
  scope_id     uuid GENERATED ALWAYS AS (COALESCE(project_id, workspace_id, account_id)) STORED,

  kind         knowledge_kind NOT NULL,
  -- name é a chave natural dentro do escopo. Para kind='index' ele é o NOME DO
  -- REPOSITÓRIO: é assim que ReadIndex(projeto, repo) acha o mapa sem uma
  -- coluna extra que só teria valor para um dos três tipos.
  name         text NOT NULL,
  version      int NOT NULL DEFAULT 1,

  -- O conteúdo mora em UM dos dois, nunca nos dois:
  --   body       — pequeno, no Postgres, porque é o que é indexável (vetor e
  --                trigrama) e legível sem uma segunda viagem de rede;
  --   object_ref — grande, no ObjectStore pela porta, com a linha guardando só
  --                a referência. Um mapa de 4 MB dentro da linha transformaria
  --                toda leitura desta tabela numa leitura de 4 MB.
  body         text NOT NULL DEFAULT '',
  object_ref   text NOT NULL DEFAULT '',
  size_bytes   int  NOT NULL DEFAULT 0,
  -- Custo estimado em tokens, medido na ESCRITA. É o que permite ao orçamento
  -- do pacote cortar sem abrir o conteúdo de cada candidato (ADR-0012).
  est_tokens   int  NOT NULL DEFAULT 0,

  -- 1536 dimensões: o tamanho usual dos embeddings de texto de propósito geral.
  -- Trocar de modelo implica migrar a coluna E reindexar toda a memória — os
  -- vetores antigos não são comparáveis com os novos.
  embedding    vector(1536),

  meta         jsonb NOT NULL DEFAULT '{}',
  created_by   uuid REFERENCES users(id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT knowledge_escopo_coerente CHECK (
    (scope = 'account'   AND workspace_id IS NULL     AND project_id IS NULL) OR
    (scope = 'workspace' AND workspace_id IS NOT NULL AND project_id IS NULL) OR
    (scope = 'project'   AND workspace_id IS NULL     AND project_id IS NOT NULL)),
  -- Conteúdo em dois lugares é conteúdo que diverge.
  CONSTRAINT knowledge_conteudo_em_um_lugar CHECK (NOT (body <> '' AND object_ref <> '')),
  -- Regra é texto que o agente lê INTEIRO, em todo pacote: não pode estar
  -- atrás de uma referência que a montagem teria de ir buscar.
  CONSTRAINT knowledge_regra_e_inline CHECK (kind <> 'rule' OR object_ref = '')
);

-- Identidade do artefato. É também o alvo do upsert: regravar o mesmo nome no
-- mesmo escopo BUMPA a versão em vez de criar linha duplicada.
CREATE UNIQUE INDEX knowledge_artifacts_ident_idx
  ON knowledge_artifacts (account_id, kind, scope_id, name);

-- Leitura por projeto (índice da demanda, memória do projeto). O account_id
-- vem primeiro em TODO índice porque vem primeiro em toda query: conhecimento
-- que vaza entre contas é o pior defeito possível nesta plataforma.
CREATE INDEX knowledge_artifacts_project_idx
  ON knowledge_artifacts (account_id, project_id, kind)
  WHERE project_id IS NOT NULL;

-- ── busca semântica na memória (pgvector) ───────────────────────────────────
--
-- HNSW e não IVFFlat, por uma razão que é deste domínio e não de benchmark:
-- o IVFFlat particiona o espaço em listas TREINADAS sobre os dados existentes,
-- e a base de conhecimento nasce VAZIA e cresce a cada demanda encerrada.
-- Listas construídas hoje seriam construídas sobre nada, e a recall cairia à
-- medida que a memória chegasse — exigindo REINDEX periódico que ninguém vai
-- lembrar de agendar. O HNSW constrói o grafo incrementalmente, sem treino, e
-- responde bem desde a primeira linha.
--
-- O preço é real: mais memória e escrita mais lenta. Cabe aqui porque o perfil
-- é o oposto do caro — escreve-se memória no ENCERRAMENTO de uma demanda
-- (raro), e lê-se na montagem de todo pacote de contexto (constante).
--
-- Parcial em kind='memory': só a memória é buscada por similaridade. Regra e
-- índice são achados por identidade, e indexá-los custaria grafo maior para
-- responder pergunta que ninguém faz.
CREATE INDEX knowledge_artifacts_embedding_idx
  ON knowledge_artifacts USING hnsw (embedding vector_cosine_ops)
  WHERE kind = 'memory';

-- Caminho LEXICAL da mesma busca: é o que responde quando não há serviço de
-- embedding ligado. Trigrama sobre nome e corpo — pior que semântico, muito
-- melhor que devolver nada.
CREATE INDEX knowledge_artifacts_trgm_idx
  ON knowledge_artifacts USING gin ((name || ' ' || body) gin_trgm_ops)
  WHERE kind = 'memory';

-- +goose Down
DROP TABLE IF EXISTS knowledge_artifacts;
DROP TYPE IF EXISTS knowledge_scope;
DROP TYPE IF EXISTS knowledge_kind;
