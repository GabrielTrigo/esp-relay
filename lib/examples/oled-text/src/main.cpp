#include <Arduino.h>
#include <IotDevice.h>
#include "secrets.h"
#include <SPI.h>
#include <Wire.h>
#include <U8g2lib.h>

// secrets.h antigo pode não ter a macro; sem ela o certificado não é validado,
// que é o padrão da lib.
#ifndef TLS_FINGERPRINT
#define TLS_FINGERPRINT nullptr
#endif
// secrets.h antigo não tem WS_PATH: gateway na raiz.
#ifndef WS_PATH
#define WS_PATH ""
#endif

// Declare aqui, uma linha por IO, tudo o que o dispositivo expõe. A lib monta
// o `register`, aplica o `sync`, responde o `set_io` e emite o `ack` a partir
// desta tabela — não há estado de IO escrito à mão em outro lugar.
static const IotSwitchIO kSwitches[] = {
  {"Pin_D1", "LED Vermelho", D1, false, false},
  {"Pin_D5", "LED Verde",    D5, false, false},
  {"Pin_D8", "LED Amarelo",  D8, false, false},
};
static const IotNumberIO kNumbers[] = {
  {"ldr", "Luz (A0)"}
};
static const IotTextIO kTexts[] = {
  {"text", "Texto"},
};

IotDevice device({
  WIFI_SSID, WIFI_PASS, DEVICE_ID, WS_HOST, WS_PORT, API_KEY, USE_WSS,
  TLS_FINGERPRINT,
  WS_PATH,
  kSwitches, sizeof(kSwitches) / sizeof(kSwitches[0]),
  kNumbers, sizeof(kNumbers) / sizeof(kNumbers[0]),
  kTexts, sizeof(kTexts) / sizeof(kTexts[0])
});

U8G2_SSD1306_128X64_NONAME_F_SW_I2C u8g2(U8G2_R0, /* clock= */ D2, /* data= */ D4, /* reset= */ U8X8_PIN_NONE);

void setup() {
  Serial.begin(115200);
  device.begin();
  u8g2.begin();
}

static uint32_t lastRead = 0;

void loop() {
  device.loop();

  // Valor enviado pelo gateway (POST .../io com {"value": ...}). Fila de 1:
  // takeText devolve true uma vez por valor e o apaga.
  // para strings maiores que o teto, use String se couber na RAM.
  char textIn[kIotMaxReceivedTextBytes + 1];
  if (device.takeText("text", textIn, sizeof(textIn))) {
    Serial.printf("[APP] texto recebido: %s\n", textIn);
    device.publishText("text", textIn);  // ecoa de volta
    u8g2.clearBuffer();
    u8g2.setFont(u8g2_font_unifont_tf); //u8g2_font_7x14B_tr
    u8g2.drawStr(0, 15, textIn);
    u8g2.sendBuffer();
  }

  if (iotElapsed(millis(), lastRead, 1000)) {
    lastRead = millis();
    device.publishNumber("ldr", double(analogRead(A0)));
  }
}
