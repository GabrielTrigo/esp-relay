#pragma once

#include <Arduino.h>
#include <WebSocketsClient.h>
#include <ArduinoJson.h>

// IotConfig e IotApplyFn vivem em IotProtocol.h/IotDevice.h; as tabelas por
// kind (IotSwitchIO/IotNumberIO/IotTextIO) também, junto da camada de mensagens.
#include "IotProtocol.h"
#include "IotCore.h"
#include "IotTime.h"

// Logs de diagnóstico. Defina IOT_LOG_ATIVO no build (ou via build_flags) para
// vê-los no Serial; sem ele o firmware não gasta byte nem tempo com log.
#ifdef IOT_LOG_ATIVO
#define IOT_LOG(...) Serial.printf(__VA_ARGS__)
#else
#define IOT_LOG(...) ((void)0)
#endif

struct IotConfig {
  const char* ssid;
  const char* password;
  const char* deviceId;
  const char* host;
  uint16_t port;
  const char* apiKey;
  bool useWss;
  const char* fingerprint;  // nullptr = não valida o certificado
  // Prefixo HTTP antes de /ws (ex.: "/iot" atrás de Caddy em www.site/iot/).
  // nullptr ou "" = gateway na raiz. Fica no fim para não quebrar os
  // inicializadores posicionais existentes.
  const char* wsPath = nullptr;
  // Uma tabela por kind, cada IO em uma linha. Só o switch tem pino e
  // activeLow/initial; IO de valor é só identidade.
  const IotSwitchIO* switches = nullptr;
  size_t switchCount = 0;
  const IotNumberIO* numbers = nullptr;
  size_t numberCount = 0;
  const IotTextIO* texts = nullptr;
  size_t textCount = 0;
};

// Responde a um comando recebido. Só vale dentro do handler: o objeto morre
// quando o handler retorna e não deve ser guardado.
class IotReply {
 public:
  void send(JsonObjectConst msg);
  void ack(bool state, const char* io);
  void reject(const char* code, const char* io);

  IotReply(const IotReply&) = delete;
  IotReply& operator=(const IotReply&) = delete;

 private:
  friend class IotDevice;
  IotReply(void* owner, const char* commandId) : owner_(owner), commandId_(commandId) {}
  void* owner_;
  const char* commandId_;
};

typedef bool (*IotApplyFn)(const IotIO& io, bool desired);
typedef bool (*IotCommandFn)(JsonObjectConst cmd, IotReply& reply);

class IotDevice {
 public:
  static const uint8_t kMaxHandlers = 8;

  explicit IotDevice(const IotConfig& cfg, IotApplyFn apply = nullptr);
  bool on(const char* type, IotCommandFn fn);
  void begin();
  void loop();
  bool state(const char* io);
  void publishState(const char* io, bool state);

  // Publica um valor de IO `number`/`text`. A lib não lê hardware: o valor vem
  // de quem chama (analogRead, sensor I2C, contador, string). IO de kind
  // diferente do esperado, valor não finito ou texto acima de kIotMaxTextBytes
  // são recusados com log; sem conexão, o valor é descartado (não é comando).
  void publishNumber(const char* io, double value);
  void publishNumber(const char* io, float value) { publishNumber(io, (double)value); }
  // Sem esta, `analogRead(A0)` (int) é ambíguo entre double e float.
  void publishNumber(const char* io, int value) { publishNumber(io, (double)value); }
  void publishText(const char* io, const char* value);

  // Consome o último valor recebido do gateway para um IO de valor (fila de 1:
  // um valor novo substitui o pendente anterior). Devolve false se não havia
  // nada, se o IO não existe ou se o kind não bate. Sem valor até receber:
  // após o boot, o primeiro take devolve false.
  bool takeText(const char* io, char* out, size_t len);
  bool takeText(const char* io, String& out);  // conveniência
  bool takeNumber(const char* io, double& out);

 private:
  friend class IotReply;

  static const uint8_t kMaxIOs = 16;

  struct Handler {
    const char* type;
    IotCommandFn fn;
  };

  IotConfig cfg_;
  IotApplyFn apply_;
  Handler handlers_[kMaxHandlers];
  uint8_t handlerCount_;
  WebSocketsClient ws_;
  // Buffer do header de autenticação. A lib copia para um String interno, mas
  // manter o texto vivo aqui evita depender desse detalhe de implementação.
  String authHeader_;
  // Fila de saída: mensagens montadas dentro de callbacks só são escritas no
  // loop(), fora do ws_.loop(). Escrever durante o callback reentra no parser
  // da lib (clientIsConnected -> clientDisconnect -> WStype_DISCONNECTED).
  static const uint8_t kMaxPendingOut = 4;
  String pendingOut_[kMaxPendingOut];
  uint8_t pendingCount_;
  uint32_t wifiAttemptMs_;
  uint32_t lastPingMs_;
  uint32_t lastSeenMs_;
  uint32_t reconnectMs_;
  bool connected_;

  // O núcleo guarda o estado efetivo e decide o ack; o IotDevice só liga as
  // pontas (hardware e socket).
  IotCore core_;

  static bool coreApply(void* ctx, const IotIO& io, bool desired);
  static void coreEmit(void* ctx, JsonDocument& doc);

  void onWsEvent(WStype_t type, uint8_t* payload, size_t length);
  void logPreflight();
  void sendDoc(JsonDocument& doc);   // enfileira; não escreve no socket
  void flushPending();               // drena a fila fora do callback
  void handleText(const char* raw);
  void handleRegister();
  void dispatch(const char* type, JsonObjectConst doc);
  bool applyIO(const IotIO& io, bool desired);
};
