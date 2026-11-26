# esp-relay — gateway (Go)

Servidor leve em Go que media o controle remoto de IOs de ESP8266/ESP32 via WebSocket.
Faz parte do [esp-relay](../README.md); sozinho ele é apenas transporte + estado.

## Requisitos

- Go 1.22+
- Um firmware usando a [lib IotDevice](../lib) apontando para este servidor

## Configuração

| Variável | Obrigatório | Padrão | Descrição |
|---|---|---|---|
| `IOT_API_KEY` | **sim** | — | API Key global. O servidor não inicia sem ela. |
| `LISTEN_ADDR` | não | `:8080` | Endereço de escuta HTTP. |
| `STATE_FILE` | não | `data/state.json` | Caminho do estado persistido. |

```bash
export IOT_API_KEY="sua-chave-forte"
go run .
# ou
go build -o esp-relay-gateway . && ./esp-relay-gateway
```

O arquivo `data/state.json` é criado automaticamente no primeiro start.

## Endpoints

| Método | Rota | Auth | Descrição |
|---|---|---|---|
| GET | `/` | não | Painel web (API Key informada no campo). |
| GET | `/events` | sim | Stream SSE do estado para o painel (tempo real). |
| GET | `/health` | não | `{"status":"ok"}`. |
| GET | `/api/devices` | sim | Lista dispositivos, IOs e fila pendente. |
| GET | `/api/devices/{id}` | sim | Detalhes do dispositivo (`404` se não existir). |
| PATCH | `/api/devices/{id}` | sim | Renomeia: `{"name":"ESP-01 Sala"}`. |
| POST | `/api/devices/{id}/io` | sim | Altera/cria switch: `{"io":"gpio2","state":true,"name":"LED"}`; envia valor a IO de valor: `{"io":"st","value":"oi"}`. |
| DELETE | `/api/devices/{id}/io/{io}` | sim | Remove o IO do console/API (`404` se o IO não existir). |
| GET | `/ws?device_id=<id>` | sim | Socket do dispositivo. |

Autenticação: header `X-API-Key`, `Authorization: Bearer <chave>` ou `?api_key=` (evite a query em produção). Requisições sem chave válida recebem `401`. Métodos não suportados recebem `405`.

### Resposta do POST de IO

`200 OK` quando o `set_io` foi escrito no socket agora; `202 Accepted` quando não
foi entregue (enfileirado ou falha de envio).

```json
{"device_id":"esp01-sala","io":"gpio2","state":true,"delivered":true,"queued":false,"command_id":"cmd-123"}
```

Para IO de valor a resposta troca `state` por `value`:

```json
{"device_id":"esp01-sala","io":"st","value":"oi","delivered":true,"queued":false,"command_id":"cmd-124"}
```

- `delivered`: escrito com sucesso no socket do ESP neste instante.
- `queued`: comando entrou na `pending_queue` (ESP offline). É independente de `delivered`.
- A fila guarda **apenas** comandos não entregues; ao reconectar, o ESP recebe `sync`,
  o servidor drena a fila e remove os itens somente após o `ack`.
- Para IO de valor a fila é de 1: um novo valor do mesmo IO substitui o pendente.

### Remoção de IO (`DELETE .../io/{io}`)

Remove o IO **do console/API** e do `state.json`. O firmware não é alterado: não
existe comando de remoção no protocolo, então enquanto o ESP anunciar o IO no
`register` ele volta na próxima conexão (e, para IO de valor, no próximo
`report`). Para apagar de vez, grave o firmware sem o IO.

```json
{"device_id":"esp01-sala","io":"gpio2","delivered":false,"queued":false,"command_id":"cmd-125"}
```

- As pendências do IO removido saem da fila junto.
- Se era um switch **ligado**, o gateway envia (ou enfileira) um `set_io` de
  volta para `false`: o relé não pode ficar ligado por um botão que sumiu do
  painel. `queued:true` (e `202 Accepted`) indica que esse desligamento espera a
  reconexão.
- O `ack` desse desligamento drena a pendência e **não** recria o IO.

## WebSocket

Mensagens JSON com `"type"`:

- ESP → Servidor `register`: `{"type":"register","device_id":"...","ios":{"gpio2":{"name":"LED da Placa","kind":"switch","state":false}}}`
  — o firmware anuncia os IOs suportados ao conectar; o servidor cria os que faltam
  (preservando o estado desejado já salvo) e responde com `sync`. É assim que o painel
  passa a listar os IOs sem criação manual.
- Servidor → ESP `sync`: `{"type":"sync","device_id":"...","ios":{...}}` — só IOs
  `switch`; IO `number`/`text` não tem estado a aplicar e fica de fora.
- Servidor → ESP `set_io`: `{"type":"set_io","command_id":"cmd-1","io":"gpio2","state":true}`
  para `switch`; para IO de valor, `{"type":"set_io","command_id":"cmd-2","io":"st","value":"oi"}`
  (`state` continua presente como `false` por compatibilidade e é ignorado).
- ESP → Servidor `ack`: `{"type":"ack","command_id":"cmd-1","io":"gpio2","state":true}`
- ESP → Servidor `report`: `{"type":"report","io":"ldr","kind":"number","value":734}`
  (ou `"kind":"text"` com string). Ver [IOs de valor](#ios-de-valor-number-e-text).
- ESP → Servidor `ping` ⟷ Servidor → ESP `pong` (atualiza `last_seen` em memória).
  O cliente envia `ping` a cada 25s.

### IOs de valor (`number` e `text`)

Cada IO do `register` tem um `kind`: `switch` (o padrão quando ausente), `number`
ou `text`. `switch` carrega `state` e é o único que o painel comanda. IO de valor
tem dois fluxos:

- **ESP → servidor**: o dispositivo publica com `report`; o servidor atualiza o
  IO e emite o `/events` na hora.
- **Servidor → ESP**: `POST /api/devices/{id}/io` com `{"value": ...}` envia o
  valor ao dispositivo. Ele não vira estado: o ESP consome com `takeText`/
  `takeNumber` no `loop()` e decide o que fazer.

- `report` exige `kind` igual ao do IO no `register`; valor que não bate com o kind
  (texto em `number`, número em `text`, `null`, NaN/Inf) é descartado com log.
- Texto de `report` tem teto de **4096 bytes**.
- Texto de `POST .../io` (`value`) tem teto de **128 bytes** — espelho de
  `kIotMaxReceivedTextBytes` da lib (buffer fixo no ESP). Acima disso → `400`.
- O valor **nunca** é gravado em `state.json` — só o `kind`. Depois de reiniciar o
  gateway, a linha do IO continua no painel mostrando `—` até o próximo report.
- `report` não passa pela `pending_queue` e não pede `ack`.
- `POST .../io` de valor **não** emite SSE: o painel só reflete o valor quando o
  ESP publicar um `report`. A exceção é a fila: com o ESP offline, o comando
  fica pendente (fila de 1, o último valor substitui o anterior) e é entregue na
  reconexão.
- O `ack` de um `set_io` de valor drena a pendência mas **não** grava `state`/
  `updated_at` — IO de valor não tem estado.

No JSON do IO: `switch` → `{"name","kind","state","updated_at"}`;
`number`/`text` → `{"name","kind","value","value_updated_at"}` (sem `state`).

#### Validação do `POST /api/devices/{id}/io`

| Corpo | Kind do IO | Resultado |
|---|---|---|
| `{"state":bool}` | switch ou inexistente | fluxo atual (cria switch se não existir) |
| `{"state":bool}` | `number`/`text` | `400` |
| `{"value":...}` | `number`/`text` | valida tipo/teto e envia |
| `{"value":...}` | switch | `400` |
| `{"value":...}` | inexistente | `400` (sem register não há kind) |
| `{"state":...,"value":...}` | qualquer | `400` (mutuamente exclusivos) |

Conexões sem mensagem por 90s são encerradas. Um novo socket para o mesmo
`device_id` encerra o anterior.

### `ack` espontâneo (sem `command_id`)

O `ack` também serve para o **dispositivo anunciar uma mudança local** (botão
físico, sensor). Sem `command_id`, o servidor grava o estado do IO e notifica os
painéis por SSE, **sem tocar na `pending_queue`**. É independente do ack de
resposta a `set_io`, que continua obrigatório para drenar a fila.

### Detalhes que importam para quem escreve firmware

- `ack` com `io` desconhecido **cria** a entrada com o nome igual à chave.
- `ack` sem `io` só é resolvido casando o `command_id` na fila; sem os dois, é
  descartado em silêncio.
- `set_io` entregue a um dispositivo **online** não entra na `pending_queue`: se o
  ack se perder no caminho, nada o reenvia. O estado do hardware só é corrigido no
  próximo `sync` ou comando.
- `state` usa `omitempty` no servidor, então o firmware deve sempre enviar a
  chave, inclusive quando for `false`.
- Comandos além de `set_io` (ex.: `set_volume`) não têm rota de emissão hoje: o
  `POST /api/devices/{id}/io` sempre manda `set_io`.

## Painel web

Abre em `/`. Ao entrar pela primeira vez, pede a API Key e a guarda no
`localStorage`. Também aceita abrir com `/#key=<chave>` para já configurar (o
fragmento não vai para logs do servidor).

- Visão de frota: totais de online/offline e comandos na fila.
- Atualiza em tempo real via SSE (`/events`); se indisponível, cai para polling de 5s.
- Toggle por IO `switch`, renomear dispositivo, adicionar IO manualmente e
  remover IO do console (botão na linha do IO, com confirmação).
- IO `number`/`text` mostra o último valor (e o "há Ns" do último report) em
  vez do toggle; o valor é atualizado sem recriar o card.
- O card só é reescrito quando algo visível muda: `last_seen` (ping a cada
  poucos segundos) não conta. Por isso o "adicionar IO" aberto e o texto
  digitado sobrevivem às atualizações; a confirmação de um switch clicado vive
  fora do DOM e é reaplicada se o card for reescrito.
- Filtro por nome/id e estado vazio explicativo.
- Tema claro/escuro pelo botão no cabeçalho; a escolha fica em `localStorage`
  (`iot_theme`) e o **claro** é o padrão. No CSS, o tema claro vem das classes
  padrão (`paper-*`, `slate-*`) e o escuro dos mesmos nomes com sufixo `dark:` —
  classes novas em `index.html` precisam do par claro/`dark:` para os dois temas.

O HTML é estático (Tailwind CDN + Google Fonts) e embutido no binário via `go:embed`.

## Deploy

Em produção, exponha atrás de um proxy TLS (Caddy, Nginx, Cloudflare Tunnel) e habilite
`WSS` no firmware (`USE_WSS true` em `secrets.h`). A API Key nunca deve trafegar em
HTTP puro.

## Build para o servidor (Windows)

```powershell
cd gateway
.\build.ps1              # go vet + go test + compila em dist\esp-relay-gateway.exe
.\build.ps1 -SkipTests   # só compila
```

Cada build recebe uma versão única com horário UTC−03:00, embutida no executável e
exibida no rodapé do painel. O `deploy.ps1` passa a mesma versão ao build, permitindo
confirmar no console qual versão foi publicada.

O binário é **auto-contido**: o painel (`web/index.html`) está embutido via
`go:embed`, então **não** é preciso copiar o HTML nem nenhum outro arquivo para o
servidor — só o `esp-relay-gateway.exe`. Mantenha o estado (`STATE_FILE`) fora da
pasta do executável, para que uma troca de binário não o sobrescreva.

### Serviço Windows + deploy por SSH

O repositório não presume uma infraestrutura sua: o `deploy.ps1` recebe tudo por
parâmetro e o servidor precisa de um serviço apontando para o `.exe`. Esboço do
serviço (rodar uma vez, como administrador, no servidor):

```powershell
New-Service -Name esp-relay -BinaryPathName "D:\apps\esp-relay\esp-relay-gateway.exe" `
            -StartupType Automatic `
            -DisplayName "esp-relay gateway"
# variáveis de ambiente do serviço (ex.: IOT_API_KEY) vão no registro do serviço
# ou num .ps1 de inicialização; ajuste o usuário/log conforme sua política.
Restart-Service esp-relay
```

Build + envio + restart + health check, num comando:

```powershell
.\deploy.ps1 -RemoteHost servidor.exemplo.com -RemoteUser usuario -Key C:\caminho\id_ed25519 -AppDir D:\apps\esp-relay
.\deploy.ps1 ... -SkipTests   # sem vet/testes
.\deploy.ps1 ... -NoBuild     # envia o dist\ existente
.\deploy.ps1 ... -WhatIf      # mostra o que faria, sem tocar no servidor
```

Parâmetros: `Key`, `RemoteHost`, `RemoteUser` e `AppDir` são obrigatórios; `SshPort`
(padrão 22), `Service` (`esp-relay`) e `HealthPort` (8080) têm padrão. O script envia
para `C:\temp`, para o serviço, troca o `.exe` (o arquivo em execução fica travado no
Windows), inicia de novo e falha com código de saída se o serviço não estiver `Running`
ou o `/health` não responder.

O fluxo normal gera uma versão nova a cada execução. Com `-NoBuild`, o executável
existente é enviado sem recompilação e mantém a versão já embutida nele.

Manual, se precisar:

```powershell
# no servidor
Stop-Service esp-relay
Copy-Item esp-relay-gateway.exe D:\apps\esp-relay\ -Force
Start-Service esp-relay
Get-WinEvent -LogName Application -MaxEvents 20   # últimas mensagens do serviço
```

## Testes

```bash
go test ./...
# corrida de dados (requer CGO/gcc):
CGO_ENABLED=1 go test -race ./...
```

O teste de regressão do painel (build tag `ui`) abre o Chrome via CDP e faz parte da
suíte normal; ele é pulado quando não há Chrome em `127.0.0.1:9222`, servidor em
`BASE_URL` ou a API Key em `UI_KEY`:

```bash
go test -tags ui -run TestUISwitchNaoRecriaCard -v
```

## Estrutura

```
main.go              servidor, hub, handlers HTTP/WS, persistência atômica
web/index.html       painel web embutido (go:embed)
data/state.json      estado de runtime (não versionado)
build.ps1            build do binário Windows
deploy.ps1           build + publicação via SSH (todos os dados por parâmetro)
```
