# IotDevice

Biblioteca C++ para ESP8266 que transforma uma placa num cliente do
[esp-relay](../README.md). Ela concentra o contrato com o gateway (`register`,
`sync`, `set_io`, `ack`, `ping/pong`), o Wi-Fi e a reconexão. Um firmware novo é
só: credenciais, **tabela de IOs** e (se precisar) handlers de comando.

## Instalar

No `platformio.ini` do seu projeto, declare a lib como dependência de pasta —
`lib_extra_dirs` varre o diretório inteiro e compilaria os `.pio` internos dos
exemplos deste repositório:

```ini
lib_deps =
  links2004/WebSockets@^2.7.3
  bblanchon/ArduinoJson@^7.4.3
  symlink://<caminho-do-repositorio-esp-relay>/lib
```

Os projetos em [`examples/`](examples) já fazem isso com caminho relativo:

| Exemplo | O que mostra |
|---|---|
| [`led-blink/`](examples/led-blink) | Mínimo: um switch e um IO `number`. É o projeto dos testes nativos. |
| [`oled-text/`](examples/oled-text) | LEDs, LDR e OLED: três switches, IO de valor nos dois sentidos. |

Cada exemplo tem `src/secrets.example.h`; copie para `src/secrets.h` e preencha.


## Uso mínimo

```cpp
#include <IotDevice.h>
#include "secrets.h"

static const IotSwitchIO kSwitches[] = {
  {"gpio2", "LED da Placa", LED_BUILTIN, /*activeLow=*/true, /*initial=*/false},
};

IotDevice device({
  WIFI_SSID, WIFI_PASS, DEVICE_ID, WS_HOST, WS_PORT, API_KEY, USE_WSS,
  nullptr,
  "",  // wsPath: "" = gateway na raiz
  kSwitches, sizeof(kSwitches) / sizeof(kSwitches[0]),
  nullptr, 0,   // IotNumberIO
  nullptr, 0    // IotTextIO
});

void setup() { Serial.begin(115200); device.begin(); }
void loop()  { device.loop(); }
```

Adicionar um IO é uma linha na tabela do kind dele. Não há `applyPhysicalIO`,
`currentIOState` nem `sendRegister` para manter em sincronia: `register`,
`sync`, `set_io` e `ack` saem todos da mesma tabela.

## Tabela de IOs

Há uma struct por kind, e ela só tem os campos que o kind usa:

| Struct | Campos |
|---|---|
| `IotSwitchIO` | `id`, `name`, `pin`, `activeLow`, `initial` |
| `IotNumberIO` | `id`, `name` |
| `IotTextIO` | `id`, `name` |

| Campo | Significado |
|---|---|
| `id` | Chave usada no JSON e no gateway (ex.: `gpio2`). |
| `name` | Rótulo exibido no painel. |
| `pin` | GPIO do switch. `-1` = virtual, exige `apply`. |
| `activeLow` | Nível aceso invertido (GPIO2 do NodeMCU é ativo em LOW). |
| `initial` | Estado aplicado no boot e anunciado no `register`. |

O driver embutido faz `digitalWrite(pin, activeLow ? !state : state)` — a
inversão mora na lib, não no seu código. IO de valor **não tem** pino, inversão
nem estado inicial: quem lê o hardware é o projeto.

## Hardware que não é pino (servo, PWM, relé com feedback)

Passe `apply` ao construir. Ela recebe o **estado lógico** e devolve o estado
efetivo (o que o `ack` vai reportar):

```cpp
static bool applyRele(const IotIO& io, bool desired) {
  digitalWrite(io.pin, desired ? HIGH : LOW);
  return desired && releConfirmou();  // devolve o que realmente aconteceu
}

IotDevice device(cfg, applyRele);
```

## Comandos além de IO

O dispatcher só recebe `type` que a lib **não** trata — `sync`, `set_io`,
`register`, `ack`, `ping` e `pong` são reservados.

```cpp
static bool onSetVolume(JsonObjectConst cmd, IotReply& reply) {
  int v = (int)(cmd["value"] | -1);
  if (v < 0 || v > 100) { reply.reject("bad_value", "volume"); return true; }
  analogWrite(SPEAKER_PIN, v);
  reply.ack(true, "volume");
  return true;
}

void setup() { device.on("set_volume", onSetVolume); device.begin(); }
```

`IotReply` só vale dentro do handler. `ack(state, io)` exige `io`; `reject`
confirma o estado atual para não deixar a fila do gateway presa.

**Limitação atual:** o gateway não tem rota que emita `set_volume` —
`POST /api/devices/{id}/io` só manda `set_io`. O dispatcher é preparação; o lado
do servidor ainda não existe.

## IOs de valor (ler e publicar)

A lib **não lê hardware**: nada de `analogRead`, I2C ou calibração aqui. Para
um IO `number`/`text`, quem produz o valor é o firmware do projeto e entrega
pronto:

```cpp
static const IotSwitchIO kSwitches[] = {
  {"gpio2", "LED", LED_BUILTIN, /*activeLow=*/true, /*initial=*/false},
};
static const IotNumberIO kNumbers[] = {
  {"ldr", "Luz"},
};
static const IotTextIO kTexts[] = {
  {"st", "Status"},
};

void loop() {
  device.loop();
  if (iotElapsed(millis(), lastRead, 1000)) {
    lastRead = millis();
    device.publishNumber("ldr", analogRead(A0));  // qualquer número serve
  }
  device.publishText("st", wifiStatusTexto());
}
```

- `publishNumber(io, double|float)` e `publishText(io, const char*)` enviam um
  `report` ao gateway; ele aparece no painel na hora e **não** grava o
  `state.json`.
- Kind trocado (`publishNumber` num `text`), `NaN`/`Inf` ou texto acima de
  **4096 bytes** são recusados com log. Sem conexão, o valor é descartado: é
  instantâneo, não é comando pendente.
- IO de valor não recebe `pinMode` nem passa por `sync`/`set_io` de estado. Não
  existe `pin` na tabela: quem amostra o hardware é você.
- O ritmo é seu: a lib não tem timer. Chame quando fizer sentido — mas cada
  report vira um evento SSE, então não chame a 50 Hz para um LDR.

## Receber valor do gateway (`takeText`/`takeNumber`)

O gateway também manda valor: `POST /api/devices/{id}/io` com
`{"io":"st","value":"mostrar menu"}` vira um `set_io` com `value`, e a lib
guarda o último valor pendente de cada IO de valor. O `loop()` consome quando
quiser:

```cpp
char texto[kIotMaxReceivedTextBytes + 1];
if (device.takeText("st", texto, sizeof(texto))) {
  // exibiu no OLED / comparou com um comando / publicou de volta
}
double n;
if (device.takeNumber("ldr", n)) {
  // usou o número recebido
}
```

- **Fila de 1, consumida:** `take*` devolve `true` uma vez e apaga o valor. Se
  dois valores chegarem antes do `take`, só o último fica.
- **Sem valor até receber:** depois do boot, `take*` devolve `false` até o
  gateway mandar algo (não há replay do último valor no `register`).
- **Teto de 128 bytes** (`kIotMaxReceivedTextBytes`) por IO de texto. O gateway
  recusa no `POST` acima disso; se um texto maior chegar mesmo assim (firmware
  e gateway fora de sincronia), a lib trunca sem avisar.
- Kind errado, IO inexistente ou valor inválido: nada fica pendente, mas a lib
  responde `ack` com o `command_id` — é o que impede a fila do gateway de
  travar esperando confirmação.
- IO de valor não tem estado: `device.state("st")` e `publishState("st", ...)`
  não se aplicam a ele.

## Mudança local de estado (botão físico)

```cpp
device.publishState("gpio2", true);
```

Isso envia um `ack` sem `command_id`: o servidor grava o estado e notifica o
painel, sem tocar na `pending_queue`. **Não chame a cada leitura de sensor** —
cada chamada reescreve o `state.json` do gateway. Para leitura contínua, use um
IO `number`/`text` com `publishNumber`/`publishText`.

## Notas de implementação (para quem for mexer na lib)

Dois erros custaram horas de depuração e não são óbvios lendo o código:

- **Não amostre `millis()` antes de `ws_.loop()`.** O callback `WStype_CONNECTED`
  roda dentro dele e grava `lastSeenMs_` com um `millis()` mais novo que o seu
  `now`. A subtração em `uint32_t` estoura e o timeout dispara no mesmo ciclo.
  Reamostre depois.
- **Não consulte `ws_.isConnected()` de dentro do callback.** `sendDoc` é chamado
  em `WStype_TEXT`, quando o status interno da lib pode já ter mudado; o check
  descarta a mensagem em silêncio. Use um flag próprio (`connected_`) e deixe
  `sendTXT` devolver o resultado.

## Gotchas

- `LED_BUILTIN` no NodeMCU é GPIO2, ativo em LOW: `activeLow = true`.
- GPIO0/2/15/16 restringem o boot. A tabela não valida isso.
- IO virtual (`pin == -1`) sem `apply` é recusado.
- IO de valor nunca tem `pin`, `activeLow` ou `initial`: esses campos não existem
  na tabela do kind. Não há como "errar" isso na declaração.
- `takeText`/`takeNumber` só existem para o kind certo: chamar `takeNumber` num
  `text` devolve `false`, sem log.
- `USE_WSS` sem fingerprint **não valida** o certificado. Em rede local é
  aceitável; em produção, defina um fingerprint.
- Gateway publicado em **subpath** (ex.: Caddy em `www.site.com/iot/`) exige
  `wsPath` no `IotConfig` (`"/iot"`; vazio = raiz). O caminho é fixo `/ws`, e sem o
  prefixo o proxy manda o Upgrade para outro serviço: o ESP loga
  `handshake failed - HTTP 404` e o gateway não registra nada.
- O diagnóstico de conexão (Wi-Fi, DNS, TCP e o motivo da desconexão) só sai com
  `-DIOT_LOG_ATIVO`; a lib websockets sozinha não informa a causa da falha.
- IO que o servidor conhece e o firmware não: ignorado no `sync`, sem `ack`, e
  continua listado no painel. Se a mensagem for `set_io` de **valor** para esse
  IO, a lib responde `ack` (para o gateway não deixar a pendência presa) e
  descarta o valor.
- `set_io` entregue com o ESP online **não** é reenfileirado pelo servidor. Se o
  comando se perder no caminho, nada o reenvia — limitação do gateway atual.
- Logs só aparecem com `-DIOT_LOG_ATIVO` no build.

## Testes

```bash
cd lib/examples/led-blink
pio test -e native
```

Requer um compilador C/C++ de host (`gcc`/`g++` no PATH). No Windows:

```powershell
winget install BrechtSanders.WinLibs.POSIX.UCRT -e
```

A suíte cobre `IotProtocol` e `IotTime` (12 casos): `command_id` omitido quando
não há id, `state: false` sempre serializado, JSON inválido e o rollover de
`millis()`. O transporte (Wi-Fi, WebSocket, reconexão) só se valida em placa
real, contra o gateway.

`IotIO` vive em `IotProtocol.h`, e não em `IotDevice.h`, justamente para que a
camada de mensagens compile no host sem Arduino, Wi-Fi ou WebSocket. As tabelas
por kind também ficam lá; o core monta o `IotIO` unificado na inicialização.
