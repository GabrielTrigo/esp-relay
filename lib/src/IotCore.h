#pragma once

#include <ArduinoJson.h>

#include "IotProtocol.h"

// Núcleo do protocolo, sem Arduino e sem socket: decide o que fazer com cada
// mensagem e delega os dois efeitos ao dono (IotDevice):
//   - applyState: aplica o estado desejado ao hardware e devolve o efetivo
//   - emit:       enfileira uma mensagem pronta para o gateway
// Existe separado do transporte para que esta lógica rode nos testes do PC —
// é aqui que os bugs de contrato do ack e do sync apareceram.
class IotCore {
 public:
  typedef bool (*ApplyFn)(void* ctx, const IotIO& io, bool desired);
  typedef void (*EmitFn)(void* ctx, JsonDocument& doc);

  // Recebe as três tabelas do projeto e monta a tabela IotIO unificada na ordem
  // switches -> numbers -> texts. Contagens acima de kMaxIOs são truncadas.
  IotCore(const IotSwitchIO* switches, size_t switchCount,
          const IotNumberIO* numbers, size_t numberCount,
          const IotTextIO* texts, size_t textCount,
          ApplyFn apply, EmitFn emit, void* ctx);

  // Processa uma mensagem recebida. Devolve false se não era JSON válido.
  bool handleMessage(const char* raw);

  // Estado efetivo de um IO. `io` desconhecido devolve false.
  bool stateOf(const char* io, bool* out) const;

  // Registra o estado efetivo sem aplicar ao hardware (publishState).
  bool setStateOf(const char* io, bool state);

  int8_t indexOf(const char* io) const;
  size_t count() const { return count_; }
  const IotIO& ioAt(size_t i) const { return ios_[i]; }
  const IotIO* iosPtr() const { return ios_; }
  bool stateAt(size_t i) const { return states_[i]; }
  const bool* statesPtr() const { return states_; }

  // Valor recebido do gateway (set_io de IO de valor). Fila de 1 por IO: um
  // valor novo substitui o pendente anterior. `take*` copia e apaga; devolvem
  // false se não havia nada pendente, se o IO não existe ou se o kind não bate.
  bool takeText(const char* io, char* out, size_t len);
  bool takeTextAt(size_t idx, char* out, size_t len);
  bool takeNumber(const char* io, double& out);
  bool takeNumberAt(size_t idx, double& out);
  size_t pendingCount() const;

 private:
  void handleSync(JsonObjectConst doc);
  void handleSetIO(JsonObjectConst doc);

  // Guarda o valor pendente no índice (já validado pelo chamador). Trunca o
  // texto em kIotMaxReceivedTextBytes.
  void setPendingValue(size_t idx, const char* text, double number, bool isNumber);

  static const size_t kMaxIOs = 16;

  struct PendingValue {
    bool pending = false;
    double number = 0;
    char text[kIotMaxReceivedTextBytes + 1];
  };

  IotIO ios_[kMaxIOs];
  size_t count_;
  ApplyFn apply_;
  EmitFn emit_;
  void* ctx_;
  bool states_[kMaxIOs];
  // Só IOs de valor usam; switch nunca recebe valor.
  PendingValue pending_[kMaxIOs];
};
