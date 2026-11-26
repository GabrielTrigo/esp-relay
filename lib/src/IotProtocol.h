#pragma once

#include <stddef.h>
#include <stdint.h>
#include <ArduinoJson.h>

// Formato do valor de um IO. Descreve o tipo do valor, não a origem dele:
// um IO `number` pode ser um analogRead, um contador ou qualquer número que o
// projeto decida publicar. IO `switch` é o único controlável pelo gateway.
enum IotIOKind : uint8_t {
  IOT_IO_SWITCH = 0,
  IOT_IO_NUMBER = 1,
  IOT_IO_TEXT = 2,
};

// Teto de valor para `text`: o gateway recusa reports maiores, e um payload
// gigante a cada segundo travaria o painel.
const size_t kIotMaxTextBytes = 4096;

// Teto de texto RECEBIDO do gateway (set_io de valor). Espelhado no gateway
// (`maxSetValueTextBytes`): mudar um exige mudar o outro. É pequeno de
// propósito: OLED, SSID e nomes de comando cabem com folga, e o buffer mora na
// RAM do ESP8266.
const size_t kIotMaxReceivedTextBytes = 128;

// IO suportado pelo dispositivo, já unificado. É o tipo que o IotCore monta a
// partir das três tabelas de configuração (IotSwitchIO/IotNumberIO/IotTextIO) e
// que a camada de mensagens consome. O projeto não o preenche: declarar
// `{id, name, pin, activeLow, initial, kind}` à mão deixaria campos sem sentido
// para cada kind. Vive aqui (e não em IotDevice.h) para que a camada de
// mensagens não dependa de Arduino, Wi-Fi ou WebSocket — é o que permite
// testá-la no PC.
struct IotIO {
  const char* id;    // chave no JSON e no gateway
  const char* name;  // rótulo exibido no painel
  int8_t pin;        // -1 = virtual; exige `apply` (sempre -1 em IO de valor)
  bool activeLow;    // só faz sentido no switch
  bool initial;      // só faz sentido no switch
  IotIOKind kind = IOT_IO_SWITCH;
};

// Tabelas declaradas pelo projeto: uma linha por IO, por kind. Campo que não
// existe no kind não existe na struct — activeLow/initial/pin de hardware são
// exclusivos do switch.
struct IotSwitchIO {
  const char* id;
  const char* name;
  int8_t pin;
  bool activeLow;
  bool initial;
};

// IO de valor: a lib não lê nem controla hardware. `id` e `name` bastam.
struct IotNumberIO {
  const char* id;
  const char* name;
};

struct IotTextIO {
  const char* id;
  const char* name;
};

// Monta a mensagem `register`: anuncia os IOs suportados e seus formatos.
// IO `switch` leva `state`; IO de valor não, porque não há estado a reconciliar.
// Devolve quantos IOs foram escritos.
size_t iotBuildRegister(JsonDocument& doc, const char* deviceId,
                        const IotIO* ios, size_t count, const bool* states);

// Monta um `ack`. `commandId` nulo ou vazio omite a chave, para o servidor
// tratar como confirmação espontânea (não mexe na pending_queue).
void iotBuildAck(JsonDocument& doc, const char* io, bool state, const char* commandId);

// Monta um `report` (dispositivo -> gateway) com o valor publicado:
// `{"type":"report","io":"ldr","kind":"number","value":734}`. O `kind` vai
// junto porque o gateway não adivinha se um valor é número ou texto.
// Devolvem false, sem escrever em `doc`, para valor não finito (NaN/Inf) ou
// texto nulo/acima de kIotMaxTextBytes.
bool iotBuildReport(JsonDocument& doc, const char* io, double value);
bool iotBuildReportText(JsonDocument& doc, const char* io, const char* value);

// Nome do kind no protocolo ("switch"/"number"/"text") e o inverso.
const char* iotKindName(IotIOKind kind);
bool iotKindFromName(const char* name, IotIOKind* out);

// Desserializa `raw` em `doc`. Devolve false se o JSON for inválido.
bool iotParse(const char* raw, JsonDocument& doc);
