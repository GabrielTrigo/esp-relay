#include "IotDevice.h"
#include "IotProtocol.h"
#include "IotTime.h"

#include <string.h>

static const uint32_t kPingIntervalMs = 25000;
// O gateway derruba conexões mudas há 90s; reconectamos antes disso.
static const uint32_t kServerSilenceMs = 75000;
static const uint32_t kWifiRetryMs = 60000;
static const uint32_t kReconnectMinMs = 1000;
static const uint32_t kReconnectMaxMs = 30000;

// Tipos do contrato que a lib trata sozinha: um handler não pode sequestrá-los,
// senão a tabela de IOs deixa de ser a fonte única do estado.
static bool isInternalType(const char* type) {
  static const char* const kInternal[] = {"sync", "set_io", "register", "ack", "ping", "pong"};
  for (size_t i = 0; i < sizeof(kInternal) / sizeof(kInternal[0]); ++i) {
    if (strcmp(type, kInternal[i]) == 0) return true;
  }
  return false;
}

IotDevice::IotDevice(const IotConfig& cfg, IotApplyFn apply)
    : cfg_(cfg),
      apply_(apply),
      handlerCount_(0),
      pendingCount_(0),
      wifiAttemptMs_(0),
      lastPingMs_(0),
      lastSeenMs_(0),
      reconnectMs_(kReconnectMinMs),
      connected_(false),
      core_(cfg.switches, cfg.switchCount, cfg.numbers, cfg.numberCount,
            cfg.texts, cfg.textCount, &IotDevice::coreApply, &IotDevice::coreEmit, this) {
  memset(handlers_, 0, sizeof(handlers_));
}

bool IotDevice::coreApply(void* ctx, const IotIO& io, bool desired) {
  IotDevice* self = static_cast<IotDevice*>(ctx);
  return self->applyIO(io, desired);
}

void IotDevice::coreEmit(void* ctx, JsonDocument& doc) {
  static_cast<IotDevice*>(ctx)->sendDoc(doc);
}

bool IotDevice::on(const char* type, IotCommandFn fn) {
  if (type == nullptr || fn == nullptr) return false;
  if (isInternalType(type)) {
    IOT_LOG("[IOT] tipo interno não aceita handler: %s\n", type);
    return false;
  }
  for (uint8_t i = 0; i < handlerCount_; ++i) {
    if (strcmp(handlers_[i].type, type) == 0) return false;
  }
  if (handlerCount_ >= kMaxHandlers) return false;
  handlers_[handlerCount_].type = type;
  handlers_[handlerCount_].fn = fn;
  ++handlerCount_;
  return true;
}

bool IotDevice::applyIO(const IotIO& io, bool desired) {
  if (apply_ != nullptr) {
    // O driver do dispositivo recebe o estado lógico e diz o que foi aplicado.
    return apply_(io, desired);
  }
  if (io.pin >= 0) {
    digitalWrite((uint8_t)io.pin, (io.activeLow ? !desired : desired) ? HIGH : LOW);
    return desired;
  }
  IOT_LOG("[IOT] IO virtual '%s' sem apply\n", io.id);
  return desired;
}

bool IotDevice::state(const char* io) {
  bool out = false;
  core_.stateOf(io, &out);
  return out;
}

void IotDevice::publishState(const char* io, bool state) {
  if (!core_.setStateOf(io, state)) {
    IOT_LOG("[IOT] publishState de IO desconhecido: %s\n", io ? io : "(nulo)");
    return;
  }
  // ack sem command_id: grava o estado no servidor sem tocar na pending_queue.
  JsonDocument doc;
  iotBuildAck(doc, io, state, nullptr);
  sendDoc(doc);
}

// Devolve false (com log) se o IO não existe ou o kind não bate com o valor
// publicado. É o que impede `publishNumber` num switch e vice-versa.
static bool ioHasKind(const IotCore& core, const char* io, IotIOKind expected, const char* fn) {
  int8_t idx = core.indexOf(io);
  if (idx < 0) {
    IOT_LOG("[IOT] %s: IO desconhecido: %s\n", fn, io ? io : "(nulo)");
    return false;
  }
  if (core.ioAt(idx).kind != expected) {
    IOT_LOG("[IOT] %s: '%s' nao e %s\n", fn, io, iotKindName(expected));
    return false;
  }
  return true;
}

void IotDevice::publishNumber(const char* io, double value) {
  if (!ioHasKind(core_, io, IOT_IO_NUMBER, "publishNumber")) return;
  JsonDocument doc;
  if (!iotBuildReport(doc, io, value)) {
    IOT_LOG("[IOT] publishNumber: valor invalido em '%s'\n", io);
    return;
  }
  sendDoc(doc);
}

void IotDevice::publishText(const char* io, const char* value) {
  if (!ioHasKind(core_, io, IOT_IO_TEXT, "publishText")) return;
  JsonDocument doc;
  if (!iotBuildReportText(doc, io, value)) {
    IOT_LOG("[IOT] publishText: valor nulo ou acima de %u bytes em '%s'\n",
            (unsigned)kIotMaxTextBytes, io);
    return;
  }
  sendDoc(doc);
}

// Valor recebido do gateway: o core guarda a fila de 1; o device só delega.
bool IotDevice::takeText(const char* io, char* out, size_t len) {
  return core_.takeText(io, out, len);
}

bool IotDevice::takeText(const char* io, String& out) {
  char buf[kIotMaxReceivedTextBytes + 1];
  if (!core_.takeText(io, buf, sizeof(buf))) return false;
  out = buf;
  return true;
}

bool IotDevice::takeNumber(const char* io, double& out) {
  return core_.takeNumber(io, out);
}

// Diagnóstico de conexão, só com IOT_LOG_ATIVO. A lib websockets no ESP8266 é
// síncrona e engole a causa da falha (connectFailedCb só loga com DEBUG da
// lib), então o firmware testa DNS e TCP antes e reporta o que viu. Não muda o
// fluxo: o WS tenta de qualquer jeito.
void IotDevice::logPreflight() {
  IOT_LOG("[IOT] preflight %s:%u (wss=%d)\n", cfg_.host, (unsigned)cfg_.port, (int)cfg_.useWss);

  IPAddress ip;
  uint32_t t0 = millis();
  bool resolved = WiFi.hostByName(cfg_.host, ip);
  IOT_LOG("[IOT] dns: %s em %lu ms -> %s\n", resolved ? "ok" : "FALHOU",
          (unsigned long)(millis() - t0),
          resolved ? ip.toString().c_str() : "-");

  if (!resolved) return;
  // No core 8266 o connect com timeout é (ip, port, timeout) só em algumas
  // versões; setConnectionTimeout cobre as demais.
  WiFiClient probe;
  probe.setTimeout(5);  // segundos, usado por connect(ip, port)
  t0 = millis();
  bool tcp = probe.connect(ip, cfg_.port);
  if (tcp) probe.stop();
  // Com WSS a porta é TLS: o TCP cru conecta e o handshake é feito depois pela
  // lib, então uma falha aqui é só o probe não falando TLS.
  IOT_LOG("[IOT] tcp %u: %s em %lu ms%s\n", (unsigned)cfg_.port, tcp ? "ok" : "falhou",
          (unsigned long)(millis() - t0),
          (cfg_.useWss && !tcp) ? " (esperado em WSS: porta e TLS)" : "");
  // Mostra o caminho que será pedido: erro de prefixo dá 404 do site vizinho.
  String prefix = cfg_.wsPath != nullptr ? cfg_.wsPath : "";
  IOT_LOG("[IOT] ws path: %s/ws?device_id=%s\n", prefix.c_str(), cfg_.deviceId);
}

void IotDevice::begin() {
  // Olhar só a tabela de switches é o ponto da refatoração: IO de valor nunca
  // recebe pinMode, e pin/activeLow/initial nem existem na linha dele.
  for (size_t i = 0; i < cfg_.switchCount; ++i) {
    if (cfg_.switches[i].pin >= 0) pinMode((uint8_t)cfg_.switches[i].pin, OUTPUT);
  }
  for (size_t i = 0; i < core_.count(); ++i) {
    const IotIO& io = core_.ioAt(i);
    if (io.kind != IOT_IO_SWITCH) continue;
    core_.setStateOf(io.id, applyIO(io, io.initial));
  }

  WiFi.mode(WIFI_STA);
  WiFi.setAutoReconnect(true);
  WiFi.begin(cfg_.ssid, cfg_.password);
  wifiAttemptMs_ = millis();

#ifdef IOT_LOG_ATIVO
  // Espera o Wi-Fi só para o diagnóstico; em produção este bloco some e o WS
  // tenta sozinho assim que a rede subir.
  uint32_t waitStart = millis();
  while (WiFi.status() != WL_CONNECTED && millis() - waitStart < 20000) {
    delay(100);
  }
  IOT_LOG("[IOT] wifi: %s ip=%s rssi=%d\n", WiFi.status() == WL_CONNECTED ? "ok" : "FALHOU",
          WiFi.localIP().toString().c_str(), (int)WiFi.RSSI());
  if (WiFi.status() == WL_CONNECTED) logPreflight();
#endif

  // O proxy que monta o gateway em subpath (ex.: Caddy handle_path /iot/*)
  // espera o prefixo na URL; sem ele o Upgrade cai no site vizinho (404).
  String prefix = cfg_.wsPath != nullptr ? cfg_.wsPath : "";
  while (prefix.endsWith("/")) prefix.remove(prefix.length() - 1);
  String path = prefix + "/ws?device_id=" + cfg_.deviceId;
  if (cfg_.useWss) {
    // beginSSL espera o fingerprint como bytes; sem ele o certificado não é
    // validado, o que só é aceitável em rede local.
    const uint8_t* fingerprint = (const uint8_t*)cfg_.fingerprint;
    ws_.beginSSL(cfg_.host, cfg_.port, path.c_str(), fingerprint, "arduino");
  } else {
    ws_.begin(cfg_.host, cfg_.port, path.c_str());
  }
  // A chave vai no header, nunca na query, para nao vazar em log de proxy.
  authHeader_ = String("X-API-Key: ") + cfg_.apiKey;
  ws_.setExtraHeaders(authHeader_.c_str());
  ws_.onEvent([this](WStype_t type, uint8_t* payload, size_t length) {
    onWsEvent(type, payload, length);
  });
  ws_.setReconnectInterval(kReconnectMinMs);
}

void IotDevice::loop() {
  uint32_t now = millis();

  if (WiFi.status() != WL_CONNECTED && iotElapsed(now, wifiAttemptMs_, kWifiRetryMs)) {
    IOT_LOG("[IOT] Wi-Fi caiu ha 60s, reemitindo begin\n");
    wifiAttemptMs_ = now;
    WiFi.begin(cfg_.ssid, cfg_.password);
  }

  ws_.loop();

  // Fora do callback: o que os callbacks enfileiraram sai agora.
  flushPending();

  if (!ws_.isConnected()) return;

  // `ws_.loop()` pode disparar WStype_CONNECTED (que faz millis() cair no
  // futuro em relacao ao `now` capturado acima). Reamostrar aqui evita que
  // `now - lastSeenMs_` estoure e dispare o timeout falso de 75s.
  now = millis();

  if (iotElapsed(now, lastPingMs_, kPingIntervalMs)) {
    lastPingMs_ = now;
    if (!ws_.sendTXT("{\"type\":\"ping\"}")) {
      IOT_LOG("[IOT] falha ao enviar ping\n");
    }
  }

  if (iotElapsed(now, lastSeenMs_, kServerSilenceMs)) {
    IOT_LOG("[IOT] servidor silencioso por 75s, reconectando\n");
    ws_.disconnect();
  }
}

void IotDevice::onWsEvent(WStype_t type, uint8_t* payload, size_t length) {
  (void)length;
  switch (type) {
    case WStype_CONNECTED:
      IOT_LOG("[IOT] conectado em %s\n", (const char*)payload);
      connected_ = true;
      lastPingMs_ = millis();
      lastSeenMs_ = millis();
      reconnectMs_ = kReconnectMinMs;
      ws_.setReconnectInterval(reconnectMs_);
      handleRegister();
      break;

    case WStype_DISCONNECTED:
      // A lib anexa o motivo (ex.: "Connection lost") quando existe.
      IOT_LOG("[IOT] desconectado%s%s\n", payload ? ": " : "",
              payload ? (const char*)payload : "");
      connected_ = false;
      break;

    case WStype_TEXT:
      lastSeenMs_ = millis();
      if (payload != nullptr) handleText((const char*)payload);
      break;

    case WStype_ERROR:
      // A lib reconecta sozinha a cada setReconnectInterval; só espaçamos as
      // tentativas. WStype_CONNECTED devolve o intervalo para o mínimo, sem o
      // que o dispositivo ficaria preso no teto depois da primeira falha.
      reconnectMs_ = (reconnectMs_ >= kReconnectMaxMs / 2) ? kReconnectMaxMs : reconnectMs_ * 2;
      ws_.setReconnectInterval(reconnectMs_);
      IOT_LOG("[IOT] erro de conexao, proxima tentativa em %lu ms\n", (unsigned long)reconnectMs_);
      break;

    default:
      break;
  }
}

void IotDevice::sendDoc(JsonDocument& doc) {
  // Nunca escrever no socket daqui: sendDoc é chamado de dentro dos callbacks
  // WStype_CONNECTED e WStype_TEXT. Um sendTXT durante o callback chama
  // clientIsConnected(), que em caso de TCP morto executa clientDisconnect() e
  // dispara WStype_DISCONNECTED dentro do próprio callback — reentrância no
  // parser da lib. A mensagem vai para a fila e sai em flushPending().
  if (pendingCount_ >= kMaxPendingOut) {
    IOT_LOG("[IOT] fila de saida cheia, mensagem descartada\n");
    return;
  }
  serializeJson(doc, pendingOut_[pendingCount_]);
  ++pendingCount_;
}

void IotDevice::flushPending() {
  if (!connected_ || !ws_.isConnected()) {
    pendingCount_ = 0;  // sessão caiu: o sync da reconexão reconcilia o estado
    return;
  }
  for (uint8_t i = 0; i < pendingCount_; ++i) {
    if (!ws_.sendTXT(pendingOut_[i])) {
      IOT_LOG("[IOT] falha ao enviar: %s\n", pendingOut_[i].c_str());
    }
  }
  pendingCount_ = 0;
}

void IotDevice::handleRegister() {
  JsonDocument doc;
  iotBuildRegister(doc, cfg_.deviceId, core_.iosPtr(), core_.count(), core_.statesPtr());
  sendDoc(doc);
  IOT_LOG("[IOT] register enviado (%u IOs)\n", (unsigned)core_.count());
}

void IotDevice::handleText(const char* raw) {
  // O core trata `sync` e `set_io` (aplicando ao hardware e enfileirando acks).
  // O que sobra — `pong` e comandos customizados — fica com o transporte.
  JsonDocument doc;
  if (!iotParse(raw, doc)) {
    IOT_LOG("[IOT] JSON invalido: %s\n", raw);
    return;
  }

  const char* type = doc["type"].as<const char*>();
  if (type == nullptr || type[0] == '\0') return;

  if (strcmp(type, "pong") == 0) {
    IOT_LOG("[IOT] pong\n");
    return;
  }
  if (strcmp(type, "sync") == 0 || strcmp(type, "set_io") == 0) {
    core_.handleMessage(raw);
    return;
  }
  dispatch(type, doc.as<JsonObjectConst>());
}

void IotDevice::dispatch(const char* type, JsonObjectConst doc) {
  for (uint8_t i = 0; i < handlerCount_; ++i) {
    if (strcmp(handlers_[i].type, type) == 0) {
      IotReply reply(this, doc["command_id"].as<const char*>());
      handlers_[i].fn(doc, reply);
      return;
    }
  }
  IOT_LOG("[IOT] comando sem handler: %s\n", type);
}

void IotReply::send(JsonObjectConst msg) {
  IotDevice* dev = static_cast<IotDevice*>(owner_);
  JsonDocument out;
  out.set(msg);
  dev->sendDoc(out);
}

void IotReply::ack(bool state, const char* io) {
  if (io == nullptr || io[0] == '\0') {
    IOT_LOG("[IOT] ack sem io ignorado\n");
    return;
  }
  IotDevice* dev = static_cast<IotDevice*>(owner_);
  JsonDocument doc;
  iotBuildAck(doc, io, state, commandId_);
  dev->sendDoc(doc);
}

void IotReply::reject(const char* code, const char* io) {
  IOT_LOG("[IOT] comando recusado: %s\n", code ? code : "(sem codigo)");

  // O contrato não tem mensagem de erro: se havia command_id, confirmar o
  // estado atual é o que impede a pending_queue de ficar presa.
  if (commandId_ == nullptr || commandId_[0] == '\0') return;

  IotDevice* dev = static_cast<IotDevice*>(owner_);
  // Sem `io` não há como confirmar estado: um ack com `io` nulo é descartado
  // pelo servidor, então só confirmamos quando o IO é conhecido.
  if (io != nullptr && io[0] != '\0') {
    JsonDocument ackDoc;
    iotBuildAck(ackDoc, io, dev->state(io), commandId_);
    dev->sendDoc(ackDoc);
  }

  JsonDocument err;
  err["type"] = "error";
  err["code"] = code;
  if (io != nullptr) err["io"] = io;
  dev->sendDoc(err);
}
