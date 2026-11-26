#include "IotProtocol.h"

#include <cmath>
#include <string.h>

const char* iotKindName(IotIOKind kind) {
  switch (kind) {
    case IOT_IO_NUMBER:
      return "number";
    case IOT_IO_TEXT:
      return "text";
    case IOT_IO_SWITCH:
    default:
      return "switch";
  }
}

bool iotKindFromName(const char* name, IotIOKind* out) {
  if (name == nullptr || out == nullptr) return false;
  if (strcmp(name, "switch") == 0) {
    *out = IOT_IO_SWITCH;
  } else if (strcmp(name, "number") == 0) {
    *out = IOT_IO_NUMBER;
  } else if (strcmp(name, "text") == 0) {
    *out = IOT_IO_TEXT;
  } else {
    return false;
  }
  return true;
}

size_t iotBuildRegister(JsonDocument& doc, const char* deviceId,
                        const IotIO* ios, size_t count, const bool* states) {
  doc["type"] = "register";
  doc["device_id"] = deviceId;
  JsonObject list = doc["ios"].to<JsonObject>();
  for (size_t i = 0; i < count; ++i) {
    JsonObject io = list[ios[i].id].to<JsonObject>();
    io["name"] = ios[i].name;
    io["kind"] = iotKindName(ios[i].kind);
    // Só o switch tem estado booleano; mandar `false` num IO de valor faria o
    // gateway tratá-lo como algo a reconciliar.
    if (ios[i].kind == IOT_IO_SWITCH) {
      io["state"] = states[i];
    }
  }
  return count;
}

void iotBuildAck(JsonDocument& doc, const char* io, bool state, const char* commandId) {
  doc["type"] = "ack";
  if (commandId != nullptr && commandId[0] != '\0') {
    doc["command_id"] = commandId;
  }
  doc["io"] = io;
  doc["state"] = state;
}

bool iotBuildReport(JsonDocument& doc, const char* io, double value) {
  if (io == nullptr || !std::isfinite(value)) return false;
  doc["type"] = "report";
  doc["io"] = io;
  // O kind vai junto: sem ele o gateway não sabe se 734 é número ou "734".
  doc["kind"] = "number";
  doc["value"] = value;
  return true;
}

bool iotBuildReportText(JsonDocument& doc, const char* io, const char* value) {
  if (io == nullptr || value == nullptr) return false;
  // strlen pode ser otimizado para strlen$4k e voltar ao código para cadenas
  // externas no ESP; a contagem manual para no teto e evita ler além dele.
  size_t len = 0;
  while (len <= kIotMaxTextBytes && value[len] != '\0') ++len;
  if (len > kIotMaxTextBytes) return false;
  doc["type"] = "report";
  doc["io"] = io;
  doc["kind"] = "text";
  doc["value"] = value;
  return true;
}

bool iotParse(const char* raw, JsonDocument& doc) {
  if (raw == nullptr) return false;
  return !deserializeJson(doc, raw).code();
}
