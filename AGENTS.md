# AGENTS.md

Guia para agentes e contribuidores de código deste repositório. Visão geral do sistema:
[`README.md`](README.md). Referência detalhada do gateway:
[`gateway/README.md`](gateway/README.md). Referência da lib:
[`lib/README.md`](lib/README.md).

## Layout

```
gateway/main.go                 Hub, handlers HTTP/WS/SSE, persistência atômica
gateway/web/index.html          painel embutido via go:embed
gateway/*_test.go               testes HTTP/WS
gateway/ui_test.go              teste de UI em Chrome (build tag `ui`)
gateway/data/                   estado de runtime (ignorado pelo git)
lib/src/                        lib IotDevice: protocolo, transporte, dispatcher
lib/examples/led-blink/         projeto autônomo mínimo + testes Unity no host
lib/examples/oled-text/         LEDs, LDR e OLED; três switches e IO de valor
docs/api/                       coleção OpenCollection da API REST
```

## Comandos

**Gateway** (Go 1.22+) — o módulo Go fica em `gateway/`; comandos `go` **só
funcionam de dentro dessa pasta** (na raiz não há `go.mod`). `cd gateway` antes:

```bash
go test ./...                              # testes unitários/HTTP/WS
CGO_ENABLED=1 go test -race ./...          # corrida de dados (requer gcc)
go test -tags ui -run TestUISwitchNaoRecriaCard -v   # UI: exige Chrome com --remote-debugging-port=9222
go run .                                   # requer IOT_API_KEY no ambiente
go vet ./...
```

**Firmware** (`pio`) — cada exemplo é um projeto independente:

```bash
cd lib/examples/oled-text
cp src/secrets.example.h src/secrets.h   # só na primeira vez
pio run                 # compilar
pio run -t upload       # gravar
pio device monitor      # serial 115200

cd ../led-blink
pio test -e native      # testes do protocolo no PC (exige gcc/g++ no PATH)
```

Nenhum firmware compila sem `src/secrets.h`; antes de rodar builds limpos a partir de um
clone, copie o `secrets.example.h` do exemplo.

No Windows, `pio test -e native` exige um compilador de host:
`winget install BrechtSanders.WinLibs.POSIX.UCRT -e`.

## Convenções

- **Idioma:** comentários, logs, mensagens de erro e commits em **português**. Nomes de
  código (tipos, funções, campos JSON) em inglês.
- **Commits:** Conventional Commits (`feat(gateway): ...`, `fix(ui): ...`), escopo
  `gateway` / `ui` / `firmware` / `lib` / `docs`.
- **Go:** handlers retornam `405` com header `Allow` via `methodNotAllowed`; JSON de erro
  é texto plano (`http.Error`) exceto `401` em `/api/*`, que devolve `{"error":"unauthorized"}`.
- **Firmware:** um IO novo é **uma linha** na tabela do kind dele no `main.cpp` do
  exemplo: `kSwitches` (`IotSwitchIO`), `kNumbers` (`IotNumberIO`) ou `kTexts`
  (`IotTextIO`). Só se passa um `IotApplyFn` quando o hardware não é pino simples
  (servo, PWM, relé com feedback). `register`, `sync`, `set_io` e `ack` saem da tabela —
  não escreva estado de IO à mão.
- **IO de valor (`number`/`text`):** a tabela do projeto só tem `id` e `name` (não
  existe `pin`/`activeLow`/`initial`); o core monta o `IotIO` interno. A lib **não lê
  hardware**: o projeto chama `publishNumber(io, valor)` / `publishText(io, str)` para
  publicar e `takeText`/`takeNumber` no `loop()` para **consumir** valor vindo do
  gateway (`POST .../io` com `value`) — fila de 1, sem replay no boot. Esses IOs ficam
  fora de `sync`/`set_io` de estado; o valor (não o kind) nunca é persistido. `set_io`
  de valor é confirmado com `ack` só para drenar a pendência.
- **Logs:** ASCII puro dentro de `IOT_LOG` (acento em `Serial.printf` vira lixo em
  alguns monitores). `-DIOT_LOG_ATIVO` fica desligado no firmware de produção.

## Invariantes do gateway (não quebre)

- Todo timestamp persistido é **UTC** (`marshalStateLocked` converte em cópia).
- Escrita do estado é atômica: `.tmp` → `Sync` → `Close` → `os.Rename`, sempre sob
  `h.mu` com o sufixo `Locked` nas funções.
- Nunca serialize estado vivo diretamente sob `RLock`; use `cloneDevice` /
  `snapshotDevicesLocked` para não compartilhar ponteiros.
- A `pending_queue` guarda **apenas** comandos não entregues. Envio bem-sucedido não
  enfileira (o `ack` não é garantido); item só sai da fila no `ack` correspondente.
- Ao definir um IO, remova pendências contraditórias do mesmo IO antes de anexar a nova.
- `ping` atualiza `last_seen` só em memória — não reescreva o `state.json` a cada ping.
- `report` (IO `number`/`text`) emite o SSE na hora e **nunca** chama `saveAtomicLocked`:
  `marshalStateLocked(false)` (arquivo) omite `value`/`value_updated_at`;
  `marshalStateLocked(true)` (SSE/API) os mantém. Só o `kind` persiste.
- `queued` e `delivered` são independentes na resposta de `POST .../io`.
- `DELETE .../io/{io}` remove o IO só do console/API (nunca há comando de remoção no
  protocolo): apaga as pendências do IO e, se era switch ligado, envia/enfileira
  `set_io false` para o relé não ficar ligado. O `ack` desse desligamento só drena a
  fila — `handleAck` não pode recriar IO removido (só cria quando o `command_id` é
  desconhecido; IO ausente com ack explícito é ignorado).
- O painel só reescreve o card quando algo **visível** muda (`viewSig` ignora
  `last_seen`/`value`/`value_updated_at`). Não volte a usar `JSON.stringify(dev)` como
  assinatura: o ping reescrevia o card e derrubava o formulário de adicionar IO.
  O estado otimista do switch vive em `pendingToggles` (fora do DOM) e é reaplicado
  após `applyCardContent`; `applyCardContent` preserva `<details>`/inputs digitados.
- O HTML é estático e servido como está (sem `html/template`), para não colidir com `${}`.

## Armadilhas

- `secrets.h`, `*.bkp`, `gateway/data/`, `gateway/dist/` e `*.exe` são ignorados — não os
  commite nem os leia para inferir comportamento.
- `/` e `/health` são abertos de propósito. Ao mexer em auth, preserve `?api_key=` em
  `/events` (o `EventSource` do navegador não envia headers).
- `gateway/web/index.html` pode ter alterações locais em andamento; revise antes
  de substituir.
- Testes que abrem WS precisam fechar os clientes para liberar o `state.json` temporário.
- O `platformio.ini` dos exemplos declara a lib como `symlink://../../` (a pasta
  `lib/`), não com `lib_extra_dirs`: a varredura do `lib_extra_dirs` passa por cada
  pasta do repositório e compila os `.pio` internos dos exemplos junto.

## Documentação

Ao mudar rotas, mensagens WS ou variáveis de ambiente, atualize juntos: este arquivo se
o contrato/invariante mudar, `README.md` (visão geral) e `gateway/README.md` (detalhe).
