#include <unity.h>
#include <ArduinoJson.h>

#include "../../../../lib/src/IotProtocol.h"
#include "../../../../lib/src/IotTime.h"

#include "../../../../lib/src/IotCore.h"

// As fontes da lib entram por include: o env `native` não linka arquivos fora
// do diretório do projeto (o LDF não os enxerga, porque a lib é só de ESP8266).
// Com IotIO em IotProtocol.h e IotCore sem Arduino, esta unidade compila no PC.
#include "../../../../lib/src/IotProtocol.cpp"
#include "../../../../lib/src/IotCore.cpp"

// O projeto declara uma tabela por kind; o core monta a tabela IotIO unificada.
static const IotSwitchIO kSwitchRows[] = {
  {"gpio2", "LED da Placa", 2, true, false},
  {"rele", "Relé", 5, true, false},
};
static const IotNumberIO kNumberRows[] = {
  {"ldr", "Luz"},
};
static const IotTextIO kTextRows[] = {
  {"status", "Status"},
};
static const bool kStates[] = {false, true};

// ---------------------------------------------------------------------------
// Núcleo do protocolo (IotCore): a lógica que decide aplicar e confirmar, a
// mesma que roda no ESP. É onde os bugs de campo viveram (ack perdido, estado
// não confirmado). Fica nesta unidade para compartilhar as definições da lib.
// ---------------------------------------------------------------------------

struct CoreHarness {
  int appliedCount;
  char appliedId[16];
  bool appliedState;
  int emitCount;
  char lastEmit[256];
};
static CoreHarness gcore;

static bool coreApplyStub(void*, const IotIO& io, bool desired) {
  ++gcore.appliedCount;
  snprintf(gcore.appliedId, sizeof(gcore.appliedId), "%s", io.id);
  gcore.appliedState = desired;
  return desired;
}

static void coreEmitStub(void*, JsonDocument& doc) {
  ++gcore.emitCount;
  serializeJson(doc, gcore.lastEmit, sizeof(gcore.lastEmit));
}

static void resetCore() {
  memset(&gcore, 0, sizeof(gcore));
}

static IotCore makeCore() {
  return IotCore(kSwitchRows, 2, kNumberRows, 1, kTextRows, 1,
                 coreApplyStub, coreEmitStub, nullptr);
}

void test_core_monta_tres_tabelas_na_ordem_switch_number_text(void) {
  IotCore core = makeCore();

  TEST_ASSERT_EQUAL_UINT32(4, (uint32_t)core.count());
  TEST_ASSERT_EQUAL_STRING("gpio2", core.ioAt(0).id);
  TEST_ASSERT_EQUAL_INT(IOT_IO_SWITCH, core.ioAt(0).kind);
  TEST_ASSERT_TRUE(core.ioAt(0).activeLow);
  TEST_ASSERT_EQUAL_STRING("rele", core.ioAt(1).id);
  TEST_ASSERT_EQUAL_STRING("ldr", core.ioAt(2).id);
  TEST_ASSERT_EQUAL_INT(IOT_IO_NUMBER, core.ioAt(2).kind);
  TEST_ASSERT_EQUAL_INT(-1, core.ioAt(2).pin);
  TEST_ASSERT_FALSE(core.ioAt(2).activeLow);
  TEST_ASSERT_EQUAL_STRING("status", core.ioAt(3).id);
  TEST_ASSERT_EQUAL_INT(IOT_IO_TEXT, core.ioAt(3).kind);
}

void test_iotio_interno_continua_com_kind_padrao_switch(void) {
  IotIO io = {"x", "X", 4, false, false};

  TEST_ASSERT_EQUAL_INT(IOT_IO_SWITCH, io.kind);
}

// ---------------------------------------------------------------------------
// Valores recebidos: fila de 1 por IO de valor, consumida com take*.
// ---------------------------------------------------------------------------

void test_take_antes_de_qualquer_valor_devolve_false(void) {
  IotCore core = makeCore();
  char buf[16];
  double n = 0;

  TEST_ASSERT_FALSE(core.takeText("status", buf, sizeof(buf)));
  TEST_ASSERT_FALSE(core.takeNumber("ldr", n));
}

void test_take_text_copia_e_apaga(void) {
  IotCore core = makeCore();
  core.handleMessage("{\"type\":\"set_io\",\"command_id\":\"c1\",\"io\":\"status\",\"value\":\"ligar\"}");
  char buf[16];

  TEST_ASSERT_TRUE(core.takeText("status", buf, sizeof(buf)));
  TEST_ASSERT_EQUAL_STRING("ligar", buf);
  TEST_ASSERT_FALSE(core.takeText("status", buf, sizeof(buf)));
}

void test_take_text_dois_valores_so_o_ultimo_sobrevive(void) {
  IotCore core = makeCore();
  core.handleMessage("{\"type\":\"set_io\",\"io\":\"status\",\"value\":\"a\"}");
  core.handleMessage("{\"type\":\"set_io\",\"io\":\"status\",\"value\":\"b\"}");
  char buf[16];

  TEST_ASSERT_TRUE(core.takeText("status", buf, sizeof(buf)));
  TEST_ASSERT_EQUAL_STRING("b", buf);
}

void test_take_text_de_128_bytes_cabe_e_129_e_truncado(void) {
  IotCore core = makeCore();
  char big[130];
  memset(big, 'x', 128);
  big[128] = '\0';
  char cmd[256];
  snprintf(cmd, sizeof(cmd), "{\"type\":\"set_io\",\"io\":\"status\",\"value\":\"%s\"}", big);
  core.handleMessage(cmd);
  char buf[200];
  TEST_ASSERT_TRUE(core.takeText("status", buf, sizeof(buf)));
  TEST_ASSERT_EQUAL_size_t(128, strlen(buf));

  // 129 bytes: defensivo (o gateway rejeita; um firmware fora de sincronia não).
  big[128] = 'y';
  big[129] = '\0';
  snprintf(cmd, sizeof(cmd), "{\"type\":\"set_io\",\"io\":\"status\",\"value\":\"%s\"}", big);
  core.handleMessage(cmd);
  TEST_ASSERT_TRUE(core.takeText("status", buf, sizeof(buf)));
  TEST_ASSERT_EQUAL_size_t(kIotMaxReceivedTextBytes, strlen(buf));
}

void test_take_number_aceita_e_recusa_nan(void) {
  IotCore core = makeCore();
  core.handleMessage("{\"type\":\"set_io\",\"io\":\"ldr\",\"value\":523.5}");
  double n = 0;

  TEST_ASSERT_TRUE(core.takeNumber("ldr", n));
  TEST_ASSERT_FLOAT_WITHIN(0.001, 523.5, n);
  TEST_ASSERT_FALSE(core.takeNumber("ldr", n));
}

void test_kind_divergente_ignora_valor_mas_acka(void) {
  resetCore();
  IotCore core = makeCore();
  core.handleMessage("{\"type\":\"set_io\",\"command_id\":\"c9\",\"io\":\"ldr\",\"value\":\"texto\"}");
  double n = 0;

  TEST_ASSERT_FALSE(core.takeNumber("ldr", n));
  TEST_ASSERT_EQUAL_INT(1, gcore.emitCount);
  TEST_ASSERT_NOT_NULL(strstr(gcore.lastEmit, "\"command_id\":\"c9\""));
  TEST_ASSERT_NOT_NULL(strstr(gcore.lastEmit, "\"state\":false"));
}

void test_set_io_de_valor_para_io_desconhecido_acka(void) {
  resetCore();
  IotCore core = makeCore();
  core.handleMessage("{\"type\":\"set_io\",\"command_id\":\"c10\",\"io\":\"fantasma\",\"value\":\"x\"}");

  TEST_ASSERT_EQUAL_INT(0, gcore.appliedCount);
  TEST_ASSERT_EQUAL_INT(1, gcore.emitCount);
  TEST_ASSERT_NOT_NULL(strstr(gcore.lastEmit, "\"command_id\":\"c10\""));
}

void test_take_em_switch_devolve_false_e_nao_le_array_vazio(void) {
  IotCore core = IotCore(kSwitchRows, 2, nullptr, 0, nullptr, 0,
                         coreApplyStub, coreEmitStub, nullptr);
  char buf[16];
  double n = 0;

  TEST_ASSERT_FALSE(core.takeText("gpio2", buf, sizeof(buf)));
  TEST_ASSERT_FALSE(core.takeNumber("gpio2", n));
  TEST_ASSERT_FALSE(core.takeNumber("ldr", n));  // inexistente neste projeto
}

void test_teto_recebido_e_128(void) {
  TEST_ASSERT_EQUAL_UINT32(128, (uint32_t)kIotMaxReceivedTextBytes);
}

void test_core_set_io_aplica_e_confirma(void) {
  resetCore();
  IotCore core = makeCore();

  TEST_ASSERT_TRUE(core.handleMessage(
      "{\"type\":\"set_io\",\"command_id\":\"cmd-7\",\"io\":\"gpio2\",\"state\":true}"));

  TEST_ASSERT_EQUAL_INT(1, gcore.appliedCount);
  TEST_ASSERT_EQUAL_STRING("gpio2", gcore.appliedId);
  TEST_ASSERT_TRUE(gcore.appliedState);
  TEST_ASSERT_EQUAL_INT(1, gcore.emitCount);
  TEST_ASSERT_NOT_NULL(strstr(gcore.lastEmit, "\"command_id\":\"cmd-7\""));
  TEST_ASSERT_NOT_NULL(strstr(gcore.lastEmit, "\"io\":\"gpio2\""));
  TEST_ASSERT_NOT_NULL(strstr(gcore.lastEmit, "\"state\":true"));
}

void test_core_set_io_desconhecido_nao_aplica_nem_confirma(void) {
  resetCore();
  IotCore core = makeCore();

  TEST_ASSERT_TRUE(core.handleMessage(
      "{\"type\":\"set_io\",\"command_id\":\"cmd-8\",\"io\":\"gpio9\",\"state\":true}"));

  TEST_ASSERT_EQUAL_INT(0, gcore.appliedCount);
  TEST_ASSERT_EQUAL_INT(0, gcore.emitCount);
}

void test_core_sync_confirma_todos_os_ios_da_tabela(void) {
  resetCore();
  IotCore core = makeCore();

  TEST_ASSERT_TRUE(core.handleMessage(
      "{\"type\":\"sync\",\"ios\":{\"gpio2\":{\"state\":true},\"rele\":{\"state\":true}}}"));

  TEST_ASSERT_EQUAL_INT(2, gcore.appliedCount);
  TEST_ASSERT_EQUAL_INT(2, gcore.emitCount);  // um ack por IO
}

void test_core_sync_ignora_io_fora_da_tabela(void) {
  resetCore();
  IotCore core = makeCore();

  TEST_ASSERT_TRUE(core.handleMessage(
      "{\"type\":\"sync\",\"ios\":{\"gpio2\":{\"state\":true},\"fantasma\":{\"state\":true}}}"));

  TEST_ASSERT_EQUAL_INT(1, gcore.appliedCount);
  TEST_ASSERT_EQUAL_INT(1, gcore.emitCount);
  TEST_ASSERT_NULL(strstr(gcore.lastEmit, "fantasma"));
}

void test_core_ack_do_sync_omite_command_id(void) {
  resetCore();
  IotCore core = makeCore();

  core.handleMessage("{\"type\":\"sync\",\"ios\":{\"gpio2\":{\"state\":true}}}");

  TEST_ASSERT_NOT_NULL(strstr(gcore.lastEmit, "\"type\":\"ack\""));
  TEST_ASSERT_NULL(strstr(gcore.lastEmit, "command_id"));
}

void test_core_estado_efetivo_vem_do_apply(void) {
  resetCore();
  IotCore core = makeCore();

  core.handleMessage("{\"type\":\"set_io\",\"io\":\"rele\",\"state\":true}");

  bool out = false;
  TEST_ASSERT_TRUE(core.stateOf("rele", &out));
  TEST_ASSERT_TRUE(out);

  bool ignored = true;
  TEST_ASSERT_FALSE(core.stateOf("gpio9", &ignored));
}

void test_core_json_invalido_nao_quebra(void) {
  resetCore();
  IotCore core = makeCore();

  TEST_ASSERT_FALSE(core.handleMessage("{nao-json"));
  TEST_ASSERT_EQUAL_INT(0, gcore.appliedCount);
  TEST_ASSERT_EQUAL_INT(0, gcore.emitCount);
}

void test_core_tabela_maior_que_o_limite_e_truncada(void) {
  static IotSwitchIO many[20];
  static char ids[20][8];
  for (int i = 0; i < 20; ++i) {
    snprintf(ids[i], sizeof(ids[i]), "io%d", i);
    many[i].id = ids[i];
    many[i].name = ids[i];
    many[i].pin = (int8_t)i;
    many[i].activeLow = false;
    many[i].initial = false;
  }
  IotCore core(many, 20, nullptr, 0, nullptr, 0, coreApplyStub, coreEmitStub, nullptr);

  TEST_ASSERT_EQUAL_UINT32(16, (uint32_t)core.count());
}

// O ack deve reportar o estado EFETIVO, não o desejado. Só um apply que
// devolve algo diferente do pedido distingue os dois — um relé que não fecha,
// por exemplo. Sem este caso, trocar `states_[idx]` por `desired` passa batido.
static bool coreApplyRejeita(void*, const IotIO&, bool) { return false; }

void test_core_ack_reporta_estado_efetivo_nao_o_desejado(void) {
  resetCore();
  IotCore core(kSwitchRows, 2, nullptr, 0, nullptr, 0, coreApplyRejeita, coreEmitStub, nullptr);

  core.handleMessage("{\"type\":\"set_io\",\"command_id\":\"c1\",\"io\":\"rele\",\"state\":true}");

  TEST_ASSERT_NOT_NULL(strstr(gcore.lastEmit, "\"state\":false"));  // efetivo
  TEST_ASSERT_NULL(strstr(gcore.lastEmit, "\"state\":true"));       // não o pedido

  bool out = true;
  TEST_ASSERT_TRUE(core.stateOf("rele", &out));
  TEST_ASSERT_FALSE(out);
}

void test_core_estado_inicial_definido_pelo_chamador(void) {
  resetCore();
  IotCore core = makeCore();

  bool out = true;
  TEST_ASSERT_TRUE(core.stateOf("gpio2", &out));
  TEST_ASSERT_FALSE(out);  // começa desligado

  TEST_ASSERT_TRUE(core.setStateOf("gpio2", true));
  TEST_ASSERT_TRUE(core.stateOf("gpio2", &out));
  TEST_ASSERT_TRUE(out);
}

// ---------------------------------------------------------------------------

void test_register_lista_todos_os_ios(void) {
  JsonDocument doc;
  size_t n = iotBuildRegister(doc, "esp-teste", makeCore().iosPtr(), 2, kStates);

  TEST_ASSERT_EQUAL_UINT32(2, (uint32_t)n);
  TEST_ASSERT_EQUAL_STRING("register", doc["type"].as<const char*>());
  TEST_ASSERT_EQUAL_STRING("esp-teste", doc["device_id"].as<const char*>());
  TEST_ASSERT_EQUAL_STRING("LED da Placa", doc["ios"]["gpio2"]["name"].as<const char*>());
  TEST_ASSERT_FALSE(doc["ios"]["gpio2"]["state"].as<bool>());
  TEST_ASSERT_TRUE(doc["ios"]["rele"]["state"].as<bool>());
}

void test_ack_sem_id_omite_command_id(void) {
  JsonDocument doc;
  iotBuildAck(doc, "gpio2", true, nullptr);

  TEST_ASSERT_TRUE(doc["command_id"].isNull());
  TEST_ASSERT_EQUAL_STRING("ack", doc["type"].as<const char*>());
  TEST_ASSERT_EQUAL_STRING("gpio2", doc["io"].as<const char*>());
  TEST_ASSERT_TRUE(doc["state"].as<bool>());
}

void test_ack_com_id_vazio_omite_command_id(void) {
  JsonDocument doc;
  iotBuildAck(doc, "gpio2", true, "");

  // O firmware antigo mandava "", que o servidor aceita mas suja o wire.
  TEST_ASSERT_TRUE(doc["command_id"].isNull());
}

void test_ack_com_id_ecoa_command_id(void) {
  JsonDocument doc;
  iotBuildAck(doc, "gpio2", false, "cmd-1");

  TEST_ASSERT_EQUAL_STRING("cmd-1", doc["command_id"].as<const char*>());
  TEST_ASSERT_FALSE(doc["state"].as<bool>());
}

void test_ack_sempre_serializa_state_false(void) {
  // O servidor usa `state` com omitempty: a chave precisa existir mesmo false.
  JsonDocument doc;
  iotBuildAck(doc, "gpio2", false, nullptr);

  // Sem Arduino no host: buffer fixo em vez de String.
  char out[128];
  size_t len = serializeJson(doc, out, sizeof(out));
  TEST_ASSERT_GREATER_THAN(0, (int)len);
  TEST_ASSERT_NOT_NULL(strstr(out, "\"state\":false"));
}

void test_parse_rejeita_json_invalido(void) {
  JsonDocument doc;

  TEST_ASSERT_FALSE(iotParse("{nao-json", doc));
  TEST_ASSERT_FALSE(iotParse(nullptr, doc));
}

void test_parse_devolve_o_tipo(void) {
  JsonDocument doc;

  TEST_ASSERT_TRUE(iotParse("{\"type\":\"sync\"}", doc));
  TEST_ASSERT_EQUAL_STRING("sync", doc["type"].as<const char*>());
}

void test_elapsed_antes_do_intervalo(void) {
  TEST_ASSERT_FALSE(iotElapsed(1000, 500, 600));
}

void test_elapsed_depois_do_intervalo(void) {
  TEST_ASSERT_TRUE(iotElapsed(1200, 500, 600));
}

void test_elapsed_no_limite_exato(void) {
  TEST_ASSERT_TRUE(iotElapsed(1100, 500, 600));  // 600 decorridos
  TEST_ASSERT_FALSE(iotElapsed(1099, 500, 600));  // 599 decorridos
}

// Simula o laço real: o relógio é setado no connect e deve impedir o timeout
// por 75s. Reproduz o bug em que o dispositivo reconectava logo após conectar.
static bool silencioDispara(uint32_t now, uint32_t lastSeen) {
  return iotElapsed(now, lastSeen, 75000);
}

// Documenta o bug real (não guarda o loop(), que depende de hardware):
// `now` era capturado ANTES de ws_.loop(), e o callback WStype_CONNECTED
// setava lastSeenMs_ = millis() DEPOIS. Com now < lastSeen, `now - lastSeen`
// estoura para ~4 bilhões e o timeout de 75s disparava no mesmo ciclo — o
// dispositivo reconectava para sempre. A proteção real está em loop():
// reamostrar millis() depois de ws_.loop().
void test_marca_no_futuro_nao_dispara_timeout(void) {
  uint32_t nowAmostradoAntes = 5000;
  uint32_t lastSeenSetadoDepois = 5001;  // WStype_CONNECTED no meio do loop

  // O bug: comparar com o `now` velho.
  TEST_ASSERT_TRUE(iotElapsed(nowAmostradoAntes, lastSeenSetadoDepois, 75000));

  // A correção: reamostrar millis() depois do ws_.loop().
  uint32_t nowReamostrado = 5001;
  TEST_ASSERT_FALSE(iotElapsed(nowReamostrado, lastSeenSetadoDepois, 75000));
}

void test_silencio_nao_dispara_logo_apos_conectar(void) {
  uint32_t lastSeen = 1000;               // millis() no momento do connect
  TEST_ASSERT_FALSE(silencioDispara(1001, lastSeen));
  TEST_ASSERT_FALSE(silencioDispara(12000, lastSeen));
  TEST_ASSERT_FALSE(silencioDispara(75999, lastSeen));  // 74999 decorridos
  TEST_ASSERT_TRUE(silencioDispara(76000, lastSeen));   // 75000 decorridos
}

void test_silencio_apos_uptime_curto_nao_dispara(void) {
  // Placa ligada há 30s: connect aos 20s, nada recebido até agora.
  uint32_t lastSeen = 20000;
  TEST_ASSERT_FALSE(silencioDispara(30000, lastSeen));
}

void test_elapsed_detecta_intervalo_maior_que_o_restante(void) {
  // Caso que separa a comparação correta da ingênua `now >= mark + interval`:
  // 2000 ms decorridos de um intervalo de 5000 ainda não completaram, mas
  // mark + interval estoura o uint32 e vira um valor pequeno (2999), o que
  // faria a comparação ingênua dizer "já passou".
  TEST_ASSERT_FALSE(iotElapsed(0xFFFFFFFFu, 0xFFFFFFFFu - 2000u, 5000u));
}

void test_elapsed_rollover_com_intervalo_completo(void) {
  // Marca 50 ms antes do estouro e now 400 ms depois: 450 ms decorridos.
  TEST_ASSERT_TRUE(iotElapsed(400u, 0xFFFFFFCEu, 450u));
}

// ---------------------------------------------------------------------------
// IOs de valor: a lib só empacota o número/texto que o projeto entrega.
// ---------------------------------------------------------------------------

static const IotIO kKinds[] = {
  {"gpio2", "LED", 2, false, false, IOT_IO_SWITCH},
  {"ldr", "Luz", -1, false, 0, IOT_IO_NUMBER},
  {"status", "Status", -1, false, false, IOT_IO_TEXT},
};
static const bool kKindsStates[] = {false, false, false};

void test_register_emite_kind_e_omite_state_em_io_de_valor(void) {
  JsonDocument doc;
  iotBuildRegister(doc, "esp", kKinds, 3, kKindsStates);

  TEST_ASSERT_EQUAL_STRING("switch", doc["ios"]["gpio2"]["kind"].as<const char*>());
  TEST_ASSERT_EQUAL_STRING("number", doc["ios"]["ldr"]["kind"].as<const char*>());
  TEST_ASSERT_EQUAL_STRING("text", doc["ios"]["status"]["kind"].as<const char*>());
  TEST_ASSERT_TRUE(doc["ios"]["gpio2"]["state"].is<bool>());
  TEST_ASSERT_TRUE(doc["ios"]["ldr"]["state"].isNull());
  TEST_ASSERT_TRUE(doc["ios"]["status"]["state"].isNull());
}

void test_report_numerico(void) {
  JsonDocument doc;
  TEST_ASSERT_TRUE(iotBuildReport(doc, "ldr", 734.0));
  TEST_ASSERT_EQUAL_STRING("report", doc["type"].as<const char*>());
  TEST_ASSERT_EQUAL_STRING("ldr", doc["io"].as<const char*>());
  TEST_ASSERT_EQUAL_STRING("number", doc["kind"].as<const char*>());
  TEST_ASSERT_EQUAL_INT(734, doc["value"].as<int>());

  char out[128];
  serializeJson(doc, out, sizeof(out));
  TEST_ASSERT_NULL(strstr(out, "\"value\":\""));
}

void test_report_numerico_fracionado_nao_vira_texto(void) {
  char out[128];
  JsonDocument doc;
  iotBuildReport(doc, "ldr", 523.5);
  serializeJson(doc, out, sizeof(out));
  TEST_ASSERT_NOT_NULL(strstr(out, "\"value\":523.5"));
}

void test_report_texto_escapa_aspas(void) {
  char out[160];
  JsonDocument doc;
  TEST_ASSERT_TRUE(iotBuildReportText(doc, "status", "a \"b\""));
  serializeJson(doc, out, sizeof(out));
  TEST_ASSERT_NOT_NULL(strstr(out, "\"kind\":\"text\""));
  TEST_ASSERT_NOT_NULL(strstr(out, "\"value\":\"a \\\"b\\\"\""));
}

void test_report_recusa_nan_inf(void) {
  JsonDocument doc;
  TEST_ASSERT_FALSE(iotBuildReport(doc, "ldr", NAN));
  TEST_ASSERT_FALSE(iotBuildReport(doc, "ldr", INFINITY));
}

void test_report_recusa_texto_acima_de_4kb(void) {
  static char big[kIotMaxTextBytes + 2];
  memset(big, 'x', sizeof(big) - 1);
  big[sizeof(big) - 1] = '\0';

  JsonDocument doc;
  TEST_ASSERT_FALSE(iotBuildReportText(doc, "status", big));

  big[kIotMaxTextBytes] = '\0';  // exatamente 4096: aceito
  TEST_ASSERT_TRUE(iotBuildReportText(doc, "status", big));
}

void test_report_recusa_texto_nulo(void) {
  JsonDocument doc;
  TEST_ASSERT_FALSE(iotBuildReportText(doc, "status", nullptr));
}

static const IotSwitchIO kCoreSwitches[] = {
  {"gpio2", "LED", 2, false, false},
};
static const IotNumberIO kCoreNumbers[] = {
  {"ldr", "Luz"},
};

void test_core_sync_ignora_io_de_valor(void) {
  resetCore();
  IotCore core(kCoreSwitches, 1, kCoreNumbers, 1, nullptr, 0,
               coreApplyStub, coreEmitStub, nullptr);

  core.handleMessage("{\"type\":\"sync\",\"ios\":{\"gpio2\":{\"state\":true},\"ldr\":{\"state\":true}}}");

  TEST_ASSERT_EQUAL_INT(1, gcore.appliedCount);   // só o switch
  TEST_ASSERT_EQUAL_INT(1, gcore.emitCount);      // um ack, do switch
  TEST_ASSERT_EQUAL_STRING("gpio2", gcore.appliedId);
}

void test_core_set_io_em_io_de_valor_nao_aplica(void) {
  resetCore();
  IotCore core(kCoreSwitches, 1, kCoreNumbers, 1, nullptr, 0,
               coreApplyStub, coreEmitStub, nullptr);

  core.handleMessage("{\"type\":\"set_io\",\"command_id\":\"c1\",\"io\":\"ldr\",\"state\":true}");

  TEST_ASSERT_EQUAL_INT(0, gcore.appliedCount);
  // Confirma para drenar a pendência do gateway; `state` não é o canal do IO
  // de valor, então o ack vai com state:false e o valor não fica pendente.
  TEST_ASSERT_EQUAL_INT(1, gcore.emitCount);
  TEST_ASSERT_NOT_NULL(strstr(gcore.lastEmit, "\"command_id\":\"c1\""));
  double n = 0;
  TEST_ASSERT_FALSE(core.takeNumber("ldr", n));
}

void test_iotio_sem_kind_e_switch(void) {
  TEST_ASSERT_EQUAL_INT(IOT_IO_SWITCH, makeCore().ioAt(0).kind);  // inicializador de 5 valores
  IotIOKind kind = IOT_IO_TEXT;
  TEST_ASSERT_TRUE(iotKindFromName("number", &kind));
  TEST_ASSERT_EQUAL_INT(IOT_IO_NUMBER, kind);
  TEST_ASSERT_FALSE(iotKindFromName("lampada", &kind));
  TEST_ASSERT_FALSE(iotKindFromName(nullptr, &kind));
}

int main(int, char**) {
  UNITY_BEGIN();
  RUN_TEST(test_register_lista_todos_os_ios);
  RUN_TEST(test_ack_sem_id_omite_command_id);
  RUN_TEST(test_ack_com_id_vazio_omite_command_id);
  RUN_TEST(test_ack_com_id_ecoa_command_id);
  RUN_TEST(test_ack_sempre_serializa_state_false);
  RUN_TEST(test_parse_rejeita_json_invalido);
  RUN_TEST(test_parse_devolve_o_tipo);
  RUN_TEST(test_elapsed_antes_do_intervalo);
  RUN_TEST(test_elapsed_depois_do_intervalo);
  RUN_TEST(test_elapsed_no_limite_exato);
  RUN_TEST(test_marca_no_futuro_nao_dispara_timeout);
  RUN_TEST(test_silencio_nao_dispara_logo_apos_conectar);
  RUN_TEST(test_silencio_apos_uptime_curto_nao_dispara);
  RUN_TEST(test_elapsed_detecta_intervalo_maior_que_o_restante);
  RUN_TEST(test_elapsed_rollover_com_intervalo_completo);
  // Núcleo do protocolo (test_core.cpp)
  RUN_TEST(test_core_set_io_aplica_e_confirma);
  RUN_TEST(test_core_set_io_desconhecido_nao_aplica_nem_confirma);
  RUN_TEST(test_core_sync_confirma_todos_os_ios_da_tabela);
  RUN_TEST(test_core_sync_ignora_io_fora_da_tabela);
  RUN_TEST(test_core_ack_do_sync_omite_command_id);
  RUN_TEST(test_core_estado_efetivo_vem_do_apply);
  RUN_TEST(test_core_json_invalido_nao_quebra);
  RUN_TEST(test_core_tabela_maior_que_o_limite_e_truncada);
  RUN_TEST(test_core_ack_reporta_estado_efetivo_nao_o_desejado);
  RUN_TEST(test_core_estado_inicial_definido_pelo_chamador);
  RUN_TEST(test_core_monta_tres_tabelas_na_ordem_switch_number_text);
  RUN_TEST(test_iotio_interno_continua_com_kind_padrao_switch);
  // Valores recebidos
  RUN_TEST(test_take_antes_de_qualquer_valor_devolve_false);
  RUN_TEST(test_take_text_copia_e_apaga);
  RUN_TEST(test_take_text_dois_valores_so_o_ultimo_sobrevive);
  RUN_TEST(test_take_text_de_128_bytes_cabe_e_129_e_truncado);
  RUN_TEST(test_take_number_aceita_e_recusa_nan);
  RUN_TEST(test_kind_divergente_ignora_valor_mas_acka);
  RUN_TEST(test_set_io_de_valor_para_io_desconhecido_acka);
  RUN_TEST(test_take_em_switch_devolve_false_e_nao_le_array_vazio);
  RUN_TEST(test_teto_recebido_e_128);
  // IOs de valor
  RUN_TEST(test_register_emite_kind_e_omite_state_em_io_de_valor);
  RUN_TEST(test_report_numerico);
  RUN_TEST(test_report_numerico_fracionado_nao_vira_texto);
  RUN_TEST(test_report_texto_escapa_aspas);
  RUN_TEST(test_report_recusa_nan_inf);
  RUN_TEST(test_report_recusa_texto_acima_de_4kb);
  RUN_TEST(test_report_recusa_texto_nulo);
  RUN_TEST(test_iotio_sem_kind_e_switch);
  RUN_TEST(test_core_sync_ignora_io_de_valor);
  RUN_TEST(test_core_set_io_em_io_de_valor_nao_aplica);
  return UNITY_END();
}
