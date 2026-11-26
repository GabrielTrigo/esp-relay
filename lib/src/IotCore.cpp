#include "IotCore.h"

#include <cmath>
#include <string.h>

// Zera um IotIO e preenche só os campos que o kind usa. IO de valor não tem
// pino nem inversão: pin = -1, activeLow = initial = false.
static void fillSwitch(IotIO& out, const IotSwitchIO& in) {
  out.id = in.id;
  out.name = in.name;
  out.pin = in.pin;
  out.activeLow = in.activeLow;
  out.initial = in.initial;
  out.kind = IOT_IO_SWITCH;
}

static void fillNumber(IotIO& out, const IotNumberIO& in) {
  out.id = in.id;
  out.name = in.name;
  out.pin = -1;
  out.activeLow = false;
  out.initial = false;
  out.kind = IOT_IO_NUMBER;
}

static void fillText(IotIO& out, const IotTextIO& in) {
  out.id = in.id;
  out.name = in.name;
  out.pin = -1;
  out.activeLow = false;
  out.initial = false;
  out.kind = IOT_IO_TEXT;
}

IotCore::IotCore(const IotSwitchIO* switches, size_t switchCount,
                 const IotNumberIO* numbers, size_t numberCount,
                 const IotTextIO* texts, size_t textCount,
                 ApplyFn apply, EmitFn emit, void* ctx)
    : count_(0), apply_(apply), emit_(emit), ctx_(ctx) {
  memset(ios_, 0, sizeof(ios_));
  memset(states_, 0, sizeof(states_));

  // A ordem é o contrato do índice: switches, depois numbers, depois texts.
  // O truncamento em kMaxIOs vale para o total, não por tabela.
  for (size_t i = 0; i < switchCount && count_ < kMaxIOs; ++i) {
    fillSwitch(ios_[count_], switches[i]);
    ++count_;
  }
  for (size_t i = 0; i < numberCount && count_ < kMaxIOs; ++i) {
    fillNumber(ios_[count_], numbers[i]);
    ++count_;
  }
  for (size_t i = 0; i < textCount && count_ < kMaxIOs; ++i) {
    fillText(ios_[count_], texts[i]);
    ++count_;
  }
}

int8_t IotCore::indexOf(const char* io) const {
  if (io == nullptr) return -1;
  for (size_t i = 0; i < count_; ++i) {
    if (strcmp(ios_[i].id, io) == 0) return (int8_t)i;
  }
  return -1;
}

bool IotCore::stateOf(const char* io, bool* out) const {
  int8_t idx = indexOf(io);
  if (idx < 0) return false;
  if (out != nullptr) *out = states_[idx];
  return true;
}

bool IotCore::setStateOf(const char* io, bool state) {
  int8_t idx = indexOf(io);
  if (idx < 0) return false;
  states_[idx] = state;
  return true;
}

bool IotCore::handleMessage(const char* raw) {
  JsonDocument doc;
  if (!iotParse(raw, doc)) return false;

  const char* type = doc["type"].as<const char*>();
  if (type == nullptr || type[0] == '\0') return true;

  if (strcmp(type, "sync") == 0) {
    handleSync(doc.as<JsonObjectConst>());
  } else if (strcmp(type, "set_io") == 0) {
    handleSetIO(doc.as<JsonObjectConst>());
  }
  // `pong` e tipos desconhecidos não mudam estado: o transporte decide o resto,
  // e o dispatcher de comandos customizados também fica no IotDevice.
  return true;
}

void IotCore::handleSync(JsonObjectConst doc) {
  JsonObjectConst ios = doc["ios"].as<JsonObjectConst>();
  if (ios.isNull()) return;

  for (JsonPairConst kv : ios) {
    int8_t idx = indexOf(kv.key().c_str());
    // IO que o servidor conhece e o firmware não: ignora, sem ack, e ele
    // continua listado no painel.
    if (idx < 0) continue;
    // IO de valor não tem estado a aplicar: o gateway não o manda no sync, mas
    // um firmware diverge; sem ack, para não reescrever o valor com um bool.
    if (ios_[idx].kind != IOT_IO_SWITCH) continue;

    bool desired = kv.value()["state"].as<bool>();
    states_[idx] = apply_ != nullptr ? apply_(ctx_, ios_[idx], desired) : desired;

    // Confirma todos: um set_io entregue com o ESP online não entra na fila do
    // gateway, então o ack é a única forma de o servidor saber o estado real.
    JsonDocument out;
    iotBuildAck(out, ios_[idx].id, states_[idx], nullptr);
    emit_(ctx_, out);
  }
}

void IotCore::handleSetIO(JsonObjectConst doc) {
  const char* ioKey = doc["io"].as<const char*>();
  const char* cmdId = doc["command_id"].as<const char*>();
  JsonVariantConst value = doc["value"];

  int8_t idx = indexOf(ioKey);
  if (idx < 0) {
    // Valor para IO que este firmware não tem: não aplica, mas acka para o
    // gateway drenar a pendência (senão ela ficaria presa para sempre).
    // `set_io` de estado para IO desconhecido continua sem ack, como antes.
    if (!value.isNull()) {
      JsonDocument out;
      iotBuildAck(out, ioKey, false, cmdId);
      emit_(ctx_, out);
    }
    return;
  }
  if (ios_[idx].kind != IOT_IO_SWITCH) {
    // IO de valor: guarda o último valor (fila de 1) e confirma para drenar a
    // fila do gateway. `state` bool não é o canal dele.
    if (!value.isNull()) {
      if (ios_[idx].kind == IOT_IO_TEXT && value.is<const char*>()) {
        setPendingValue(idx, value.as<const char*>(), 0, false);
      } else if (ios_[idx].kind == IOT_IO_NUMBER && value.is<double>()) {
        double number = value.as<double>();
        if (std::isfinite(number)) setPendingValue(idx, nullptr, number, true);
      }
    }
    JsonDocument out;
    iotBuildAck(out, ios_[idx].id, false, cmdId);
    emit_(ctx_, out);
    return;
  }

  bool desired = doc["state"].as<bool>();
  states_[idx] = apply_ != nullptr ? apply_(ctx_, ios_[idx], desired) : desired;

  JsonDocument out;
  iotBuildAck(out, ios_[idx].id, states_[idx], cmdId);
  emit_(ctx_, out);
}

void IotCore::setPendingValue(size_t idx, const char* text, double number, bool isNumber) {
  if (idx >= count_) return;
  PendingValue& slot = pending_[idx];
  if (isNumber) {
    slot.number = number;
    slot.text[0] = '\0';
  } else {
    strncpy(slot.text, text, kIotMaxReceivedTextBytes);
    slot.text[kIotMaxReceivedTextBytes] = '\0';
    slot.number = 0;
  }
  slot.pending = true;
}

bool IotCore::takeTextAt(size_t idx, char* out, size_t len) {
  if (idx >= count_ || out == nullptr || len == 0) return false;
  if (ios_[idx].kind != IOT_IO_TEXT) return false;
  PendingValue& slot = pending_[idx];
  if (!slot.pending) return false;
  strncpy(out, slot.text, len - 1);
  out[len - 1] = '\0';
  slot.pending = false;
  return true;
}

bool IotCore::takeText(const char* io, char* out, size_t len) {
  int8_t idx = indexOf(io);
  if (idx < 0) return false;
  return takeTextAt((size_t)idx, out, len);
}

bool IotCore::takeNumberAt(size_t idx, double& out) {
  if (idx >= count_) return false;
  if (ios_[idx].kind != IOT_IO_NUMBER) return false;
  PendingValue& slot = pending_[idx];
  if (!slot.pending) return false;
  out = slot.number;
  slot.pending = false;
  return true;
}

bool IotCore::takeNumber(const char* io, double& out) {
  int8_t idx = indexOf(io);
  if (idx < 0) return false;
  return takeNumberAt((size_t)idx, out);
}

size_t IotCore::pendingCount() const {
  size_t n = 0;
  for (size_t i = 0; i < count_; ++i) {
    if (pending_[i].pending) ++n;
  }
  return n;
}
