#include <IotDevice.h>
#include "secrets.h"

// secrets.h pode não definir a macro: sem ela o certificado não é validado,
// que é o padrão da lib.
#ifndef TLS_FINGERPRINT
#define TLS_FINGERPRINT nullptr
#endif
#ifndef WS_PATH
#define WS_PATH ""
#endif

// GPIO2 é o LED da placa no NodeMCU, ativo em LOW.
// `ldr` é um IO de valor: a lib não lê o pino. O valor é lido aqui no loop() e
// entregue por publishNumber — troque o analogRead por qualquer número.
static const IotSwitchIO kSwitches[] = {
  {"gpio2", "LED da Placa", LED_BUILTIN, /*activeLow=*/true, /*initial=*/false},
};
static const IotNumberIO kNumbers[] = {
  {"ldr", "Luz (LDR)"},
};

// useWss sem fingerprint não valida o certificado (aceitável em rede local).
IotDevice device({
  WIFI_SSID, WIFI_PASS, DEVICE_ID, WS_HOST, WS_PORT, API_KEY, USE_WSS,
  TLS_FINGERPRINT,
  WS_PATH,  // "/iot" atrás de proxy em subpath; "" na raiz
  kSwitches, sizeof(kSwitches) / sizeof(kSwitches[0]),
  kNumbers, sizeof(kNumbers) / sizeof(kNumbers[0]),
  nullptr, 0
});

static uint32_t lastRead = 0;

void setup() {
  Serial.begin(115200);
  device.begin();
}

void loop() {
  device.loop();

  // Valor enviado pelo gateway (POST .../io com {"value": ...}). O exemplo não
  // tem IO `text`: o takeNumber mostra a mesma API para números.
  double ldrIn = 0;
  if (device.takeNumber("ldr", ldrIn)) {
    Serial.printf("[APP] valor recebido do gateway: %.0f\n", ldrIn);
  }

  // Amostra própria do projeto, 1x por segundo. A lib só empacota o valor.
  if (iotElapsed(millis(), lastRead, 1000)) {
    lastRead = millis();
    device.publishNumber("ldr", analogRead(A0));
  }
}
