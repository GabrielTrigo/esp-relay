package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func doRequest(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-API-Key", testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// POST para dispositivo inexistente não deve criar dispositivos silenciosamente (§6).
func TestAPI_PostIONoDispositivoInexistenteRetorna404(t *testing.T) {
	h := newTestHub(t)
	srv := newTestServer(t, h)

	status, _ := doRequest(t, srv, http.MethodPost, "/api/devices/nao-existe/io", `{"io":"gpio0","state":true}`)
	if status != http.StatusNotFound {
		t.Fatalf("esperava 404, obtive %d", status)
	}

	h.mu.RLock()
	_, created := h.data.Devices["nao-existe"]
	h.mu.RUnlock()
	if created {
		t.Fatal("dispositivo inexistente não deveria ter sido criado")
	}
}

// Rota existente com método não suportado deve responder 405 (§6).
func TestAPI_MetodoNaoSuportadoRetorna405(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)

	if status, _ := doRequest(t, srv, http.MethodDelete, "/api/devices/dev1", ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE em /api/devices/dev1: esperava 405, obtive %d", status)
	}
	if status, _ := doRequest(t, srv, http.MethodGet, "/api/devices/dev1/io", ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET em /api/devices/dev1/io: esperava 405, obtive %d", status)
	}
}

// Coleções e /health só aceitam GET; demais métodos devem responder 405 (§6).
func TestAPI_ColecaoEHealthRecusamMetodosNaoSuportados(t *testing.T) {
	h := newTestHub(t)
	srv := newTestServer(t, h)

	cases := []struct{ method, path string }{
		{http.MethodPost, "/health"},
		{http.MethodDelete, "/health"},
		{http.MethodPost, "/api/devices"},
		{http.MethodDelete, "/api/devices"},
	}
	for _, c := range cases {
		if status, _ := doRequest(t, srv, c.method, c.path, ""); status != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: esperava 405, obtive %d", c.method, c.path, status)
		}
	}
}

// Consultas (GET) não podem mutar o estado em memória: os handlers GET serializam
// sob RLock, e a serialização não pode escrever em LastSeen/UpdatedAt (§110).
func TestAPI_GetNaoMutaEstadoEmMemoria(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	before := time.Date(2020, 1, 2, 3, 4, 5, 0, time.FixedZone("BRT", -3*3600))
	h.mu.Lock()
	h.data.UpdatedAt = before
	h.data.Devices["dev1"].LastSeen = before
	h.data.Devices["dev1"].IOs["gpio0"].UpdatedAt = before
	h.mu.Unlock()
	srv := newTestServer(t, h)

	if status, _ := doRequest(t, srv, http.MethodGet, "/api/devices", ""); status != http.StatusOK {
		t.Fatalf("GET /api/devices: esperava 200, obtive %d", status)
	}
	if status, _ := doRequest(t, srv, http.MethodGet, "/api/devices/dev1", ""); status != http.StatusOK {
		t.Fatalf("GET /api/devices/dev1: esperava 200, obtive %d", status)
	}

	h.mu.RLock()
	updatedAt := h.data.UpdatedAt
	lastSeen := h.data.Devices["dev1"].LastSeen
	ioUpdatedAt := h.data.Devices["dev1"].IOs["gpio0"].UpdatedAt
	h.mu.RUnlock()
	if updatedAt.Location() != before.Location() {
		t.Fatalf("GET não deve alterar updated_at em memória; location=%v", updatedAt.Location())
	}
	if lastSeen.Location() != before.Location() || ioUpdatedAt.Location() != before.Location() {
		t.Fatalf("GET não deve alterar last_seen/io.updated_at em memória")
	}
}

// Um nome amigável pode ser definido no device sem sobrescrever IOs/fila (#10).
func TestAPI_PatchDeviceName(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)

	status, body := doRequest(t, srv, http.MethodPatch, "/api/devices/dev1", `{"name":"ESP-01 Sala"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH name: esperava 200, obtive %d (%v)", status, body)
	}
	h.mu.RLock()
	name := h.data.Devices["dev1"].Name
	ioCount := len(h.data.Devices["dev1"].IOs)
	h.mu.RUnlock()
	if name != "ESP-01 Sala" {
		t.Fatalf("nome não foi atualizado: %q", name)
	}
	if ioCount != 1 {
		t.Fatalf("PATCH não deve alterar IOs, tem %d", ioCount)
	}
}

// PATCH em device inexistente deve retornar 404.
func TestAPI_PatchDeviceInexistenteRetorna404(t *testing.T) {
	h := newTestHub(t)
	srv := newTestServer(t, h)
	if status, _ := doRequest(t, srv, http.MethodPatch, "/api/devices/nao-existe", `{"name":"x"}`); status != http.StatusNotFound {
		t.Fatalf("esperava 404, obtive %d", status)
	}
}

// Simula o que o handler GET fazia: serializar sobre um snapshot tomado sob lock.
// Não retorna ponteiro para dados compartilhados. Sem -race não detectamos a
// corrida, mas garantimos que o snapshot é desacoplado do estado vivo.
func TestAPI_GetSnapshotDesacopladoDoEstadoVivo(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")

	h.mu.RLock()
	snapshot := h.snapshotDevicesLocked()
	h.mu.RUnlock()

	h.mu.Lock()
	h.data.Devices["dev1"].IOs["gpio0"].State = true
	h.data.Devices["dev1"].Name = "alterado"
	h.mu.Unlock()

	if snapshot[0].IOs["gpio0"].State {
		t.Fatal("snapshot não deve refletir alterações feitas depois da cópia")
	}
	if snapshot[0].Name == "alterado" {
		t.Fatal("snapshot não deve compartilhar o nome do device vivo")
	}
}

// ack sem "io" também precisa refletir o estado indicado no comando (§3).
func TestAck_SemIOAtualizaEstadoDoIO(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].PendingQueue = []PendingCommand{{
		ID: "cmd-z", Type: "set_io", IO: "gpio0", State: true, CreatedAt: time.Now(),
	}}
	h.mu.Unlock()
	srv := newTestServer(t, h)

	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn) // sync
	readSetIO(t, conn)

	if err := conn.WriteJSON(map[string]any{"type": "ack", "command_id": "cmd-z", "state": true}); err != nil {
		t.Fatalf("envia ack: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.RLock()
		ioState := h.data.Devices["dev1"].IOs["gpio0"].State
		queue := len(h.data.Devices["dev1"].PendingQueue)
		h.mu.RUnlock()
		if queue == 0 && ioState {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("após ack sem io esperava state=true e fila vazia; state=%v fila=%d", ioState, queue)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Timestamps persistidos devem ser UTC para não depender do fuso do servidor (§5).
func TestPersistencia_TimestampsEmUTC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	h, err := NewHub(path, testAPIKey)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].IOs["gpio0"].UpdatedAt = time.Now().In(time.FixedZone("BRT", -3*3600))
	err = h.saveAtomicLocked()
	h.mu.Unlock()
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var loaded StoreData
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, offset := loaded.UpdatedAt.Zone(); offset != 0 {
		t.Fatalf("updated_at deveria estar em UTC, offset=%d", offset)
	}
	if _, offset := loaded.Devices["dev1"].IOs["gpio0"].UpdatedAt.Zone(); offset != 0 {
		t.Fatalf("io.updated_at deveria estar em UTC, offset=%d", offset)
	}
}

// O load normaliza os timestamps em memória para UTC no boot (§5), e a
// serialização nunca muta o estado compartilhado.
func TestStart_CarregaTimestampsParaUTC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	raw := `{
  "updated_at": "2020-01-02T03:04:05-03:00",
  "devices": {
    "dev1": {
      "id": "dev1",
      "name": "dev1",
      "last_seen": "2020-01-02T03:04:05-03:00",
      "ios": {"gpio0": {"name": "Relé", "state": true, "updated_at": "2020-01-02T03:04:05-03:00"}}
    }
  }
}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	h, err := NewHub(path, testAPIKey)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	if _, offset := h.data.UpdatedAt.Zone(); offset != 0 {
		t.Fatalf("updated_at em memória deveria estar em UTC, offset=%d", offset)
	}
	dev := h.data.Devices["dev1"]
	if _, offset := dev.LastSeen.Zone(); offset != 0 {
		t.Fatalf("last_seen em memória deveria estar em UTC, offset=%d", offset)
	}
	if _, offset := dev.IOs["gpio0"].UpdatedAt.Zone(); offset != 0 {
		t.Fatalf("io.updated_at em memória deveria estar em UTC, offset=%d", offset)
	}
}

// Firmware anuncia seus IOs no inicio da conexao; o servidor cria os que faltam
// e preserva o estado desejado ja salvo (#register).
func TestWS_RegisterCriaIOsERespondeSync(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn) // sync inicial

	reg := map[string]any{
		"type": "register",
		"ios": map[string]any{
			"gpio2": map[string]any{"name": "LED da Placa", "state": false},
			"gpio0": map[string]any{"name": "Rele", "state": true},
		},
	}
	if err := conn.WriteJSON(reg); err != nil {
		t.Fatalf("envia register: %v", err)
	}

	var syncMsg map[string]any
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msg := readJSON(t, conn)
		if msg["type"] == "sync" {
			syncMsg = msg
			break
		}
	}
	if syncMsg == nil {
		t.Fatal("esperava sync como resposta ao register")
	}
	ios, _ := syncMsg["ios"].(map[string]any)
	if len(ios) != 2 {
		t.Fatalf("sync deveria conter os 2 IOs registrados, obtive %v", ios)
	}

	h.mu.RLock()
	ioCount := len(h.data.Devices["dev1"].IOs)
	h.mu.RUnlock()
	if ioCount != 2 {
		t.Fatalf("servidor deveria ter criado 2 IOs, tem %d", ioCount)
	}
}

// O register não pode sobrescrever o estado desejado ja salvo para um IO conhecido.
func TestWS_RegisterPreservaEstadoDesejado(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].IOs["gpio0"].State = true // desejado ligado
	h.mu.Unlock()
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn) // sync

	reg := map[string]any{
		"type": "register",
		"ios": map[string]any{
			"gpio0": map[string]any{"name": "Rele", "state": false},
		},
	}
	_ = conn.WriteJSON(reg)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if msg := readJSON(t, conn); msg["type"] == "sync" {
			break
		}
	}
	h.mu.RLock()
	state := h.data.Devices["dev1"].IOs["gpio0"].State
	h.mu.RUnlock()
	if !state {
		t.Fatal("register não deve sobrescrever o estado desejado (true)")
	}
}

func TestIOState_JSONSwitchSempreTemStateValorNaoTem(t *testing.T) {
	b, _ := json.Marshal(&IOState{Name: "Relé"})
	if !strings.Contains(string(b), `"state":false`) {
		t.Fatalf("switch precisa emitir state mesmo false: %s", b)
	}
	now := time.Now().UTC()
	raw := json.RawMessage(`734`)
	b, _ = json.Marshal(&IOState{Name: "Luz", Kind: "number", Value: &raw, ValueUpdatedAt: &now})
	if strings.Contains(string(b), `"state"`) {
		t.Fatalf("IO de valor não deve emitir state: %s", b)
	}
	if !strings.Contains(string(b), `"value":734`) {
		t.Fatalf("IO de valor deve emitir value numérico: %s", b)
	}
	if !strings.Contains(string(b), `"kind":"number"`) {
		t.Fatalf("IO de valor deve emitir kind: %s", b)
	}
}

func TestWS_RegisterGravaKindEOmiteIODeValorNoSync(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn) // sync inicial

	_ = conn.WriteJSON(map[string]any{
		"type": "register",
		"ios": map[string]any{
			"gpio0": map[string]any{"name": "Relé", "state": true, "kind": "switch"},
			"ldr":   map[string]any{"name": "Luz", "kind": "number"},
			"st":    map[string]any{"name": "Status", "kind": "text"},
		},
	})

	var syncIOS map[string]any
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if msg := readJSON(t, conn); msg["type"] == "sync" {
			syncIOS, _ = msg["ios"].(map[string]any)
			break
		}
	}
	if syncIOS == nil {
		t.Fatal("esperava sync como resposta ao register")
	}
	if _, ok := syncIOS["ldr"]; ok {
		t.Fatal("sync não deve mandar IO de valor (nada a aplicar)")
	}
	if _, ok := syncIOS["st"]; ok {
		t.Fatal("sync não deve mandar IO de valor (nada a aplicar)")
	}
	if _, ok := syncIOS["gpio0"]; !ok {
		t.Fatal("sync deve mandar o switch")
	}

	h.mu.RLock()
	kind := h.data.Devices["dev1"].IOs["ldr"].Kind
	h.mu.RUnlock()
	if kind != "number" {
		t.Fatalf("kind do ldr deveria ser number, obtive %q", kind)
	}
}

func TestWS_RegisterAtualizaKindQuandoFirmwareMuda(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn)

	_ = conn.WriteJSON(map[string]any{
		"type": "register",
		"ios":  map[string]any{"gpio0": map[string]any{"name": "Relé", "kind": "number"}},
	})
	for i := 0; i < 5; i++ {
		if msg := readJSON(t, conn); msg["type"] == "sync" {
			break
		}
	}

	h.mu.RLock()
	io := h.data.Devices["dev1"].IOs["gpio0"]
	h.mu.RUnlock()
	if io.Kind != "number" {
		t.Fatalf("kind deveria ter virado number, obtive %q", io.Kind)
	}
	if io.State || !io.UpdatedAt.IsZero() {
		t.Fatalf("estado de switch deveria ter sido zerado ao virar IO de valor: %+v", io)
	}
}

func TestPersistencia_StateJSONAntigoSemKindCarregaComoSwitch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	old := `{"updated_at":"2026-01-01T00:00:00Z","devices":{"dev1":{"id":"dev1","name":"dev1","online":false,"last_seen":"2026-01-01T00:00:00Z","ios":{"gpio0":{"name":"Relé","state":true,"updated_at":"2026-01-01T00:00:00Z"}},"pending_queue":[]}}}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatalf("escreve state: %v", err)
	}
	h, err := NewHub(path, testAPIKey)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	h.mu.RLock()
	kind := h.data.Devices["dev1"].IOs["gpio0"].Kind
	h.mu.RUnlock()
	if !isSwitchKind(kind) {
		t.Fatalf("kind vazio deveria se comportar como switch, obtive %q", kind)
	}
}

func reportar(t *testing.T, conn *websocket.Conn, io string, value any) {
	t.Helper()
	// Kind acompanha o valor, como no firmware: 734 e "734" são kinds diferentes.
	kind := "number"
	switch value.(type) {
	case string:
		kind = "text"
	case nil, bool, map[string]any:
		kind = "text" // valor fora do contrato: qualquer kind serve para o teste
	}
	if err := conn.WriteJSON(map[string]any{"type": "report", "io": io, "kind": kind, "value": value}); err != nil {
		t.Fatalf("envia report: %v", err)
	}
}

// esperaPongBloqueia até o servidor responder: garante que a mensagem anterior
// (o report) já foi processada antes de conferirmos o estado.
func esperaPong(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.WriteJSON(map[string]any{"type": "ping"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		_ = conn.SetReadDeadline(deadline)
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read pong: %v", err)
		}
		if msg["type"] == "pong" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("esperava pong")
		}
	}
}

func valorEm(h *Hub, deviceID, io string) (*json.RawMessage, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	entry := h.data.Devices[deviceID].IOs[io]
	if entry == nil || entry.Value == nil {
		return nil, false
	}
	return entry.Value, true
}

func TestReport_NumeroAtualizaValorSemGravarArquivo(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].IOs["ldr"] = &IOState{Name: "Luz", Kind: "number"}
	h.mu.Unlock()
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn)

	rawBefore, _ := os.ReadFile(h.filePath)
	reportar(t, conn, "ldr", 734)
	esperaPong(t, conn)
	rawAfter, _ := os.ReadFile(h.filePath)

	if string(rawBefore) != string(rawAfter) {
		t.Fatal("report não pode reescrever o state.json")
	}
	if v, ok := valorEm(h, "dev1", "ldr"); !ok || string(*v) != "734" {
		t.Fatalf("valor deveria ser 734, obtive %v", v)
	}
}

func TestReport_TextoEAceito(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].IOs["st"] = &IOState{Name: "Status", Kind: "text"}
	h.mu.Unlock()
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn)

	reportar(t, conn, "st", "online")
	esperaPong(t, conn)
	if v, ok := valorEm(h, "dev1", "st"); !ok || string(*v) != `"online"` {
		t.Fatalf("valor deveria ser \"online\", obtive %v", v)
	}
}

func TestReport_TipoErradoNaoAlteraValor(t *testing.T) {
	cases := []struct {
		kind, io string
		value    any
	}{
		{"number", "ldr", true},
		{"number", "ldr", nil},
		{"text", "st", 734},
		{"text", "st", map[string]any{"a": 1}},
		{"text", "st", nil},
	}
	for _, c := range cases {
		h := newTestHub(t)
		withTestDevice(h, "dev1")
		if c.kind == "number" || c.kind == "text" {
			h.mu.Lock()
			h.data.Devices["dev1"].IOs[c.io] = &IOState{Name: c.io, Kind: c.kind}
			h.mu.Unlock()
		}
		srv := newTestServer(t, h)
		conn := connectWS(t, srv, "dev1")
		readJSON(t, conn)

		reportar(t, conn, c.io, c.value)
		esperaPong(t, conn)
		if v, ok := valorEm(h, "dev1", c.io); ok {
			t.Fatalf("report %q em IO %s/%s deveria ser descartado, gravou %s", c.value, c.kind, c.io, *v)
		}
		drainHub(t, h)
	}
}

// Um report number num switch do painel não pode transformá-lo em linha de valor.
func TestReport_EmIOSwitchEhDescartado(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1") // gpio0 é switch
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn)

	_ = conn.WriteJSON(map[string]any{"type": "report", "io": "gpio0", "kind": "number", "value": 7})
	esperaPong(t, conn)
	if v, ok := valorEm(h, "dev1", "gpio0"); ok {
		t.Fatalf("report em switch deveria ser descartado, gravou %s", *v)
	}
	h.mu.RLock()
	kind := h.data.Devices["dev1"].IOs["gpio0"].Kind
	h.mu.RUnlock()
	if kind == "number" {
		t.Fatal("report não pode reatribuir o kind de um IO existente")
	}
}

// O kind do register manda também no sentido valor->text: um report number num
// IO text não vira valor.
func TestReport_KindDivergenteDoIOEhDescartado(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].IOs["st"] = &IOState{Name: "Status", Kind: "text"}
	h.mu.Unlock()
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn)

	_ = conn.WriteJSON(map[string]any{"type": "report", "io": "st", "kind": "number", "value": 42})
	esperaPong(t, conn)
	if v, ok := valorEm(h, "dev1", "st"); ok {
		t.Fatalf("report de kind number em IO text deveria ser descartado, gravou %s", *v)
	}
}

// Report num IO que o register não anunciou (defesa) cria a linha e grava o
// kind uma única vez — senão ele some no restart.
func TestReport_IOCriadoPorReportPersisteOKindEEsqueceValor(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn)

	reportar(t, conn, "novo", 11)
	// Sem esperar o pong (se o IO não for criado, o pong nunca vem e o teste
	// penduraria em vez de falhar): espera o efeito no estado, com prazo.
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.RLock()
		criado := h.data.Devices["dev1"].IOs["novo"]
		h.mu.RUnlock()
		if criado != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("report não criou o IO desconhecido")
		}
		time.Sleep(20 * time.Millisecond)
	}

	gravado, _ := os.ReadFile(h.filePath)
	if !strings.Contains(string(gravado), `"kind": "number"`) {
		t.Fatalf("o kind do IO criado por report deveria ser gravado:\n%s", gravado)
	}
	if strings.Contains(string(gravado), "11") && strings.Contains(string(gravado), `"value"`) {
		t.Fatalf("o valor não pode ser gravado:\n%s", gravado)
	}

	h2, err := NewHub(h.filePath, testAPIKey)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	h2.mu.RLock()
	io := h2.data.Devices["dev1"].IOs["novo"]
	h2.mu.RUnlock()
	if io == nil || io.Kind != "number" || io.Value != nil {
		t.Fatalf("após restart esperado kind=number e valor vazio, obtive %+v", io)
	}
}

// Volta valor->switch no register: o toggle volta, o sync volta a mandar o IO e
// o valor antigo some.
func TestWS_RegisterVoltaParaSwitchDescartandoValor(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	raw := json.RawMessage(`734`)
	now := time.Now().UTC()
	h.data.Devices["dev1"].IOs["ldr"] = &IOState{Name: "Luz", Kind: "number", Value: &raw, ValueUpdatedAt: &now}
	h.mu.Unlock()
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn)

	_ = conn.WriteJSON(map[string]any{
		"type": "register",
		"ios":  map[string]any{"ldr": map[string]any{"name": "Luz", "kind": "switch", "state": true}},
	})
	var syncIOS map[string]any
	for i := 0; i < 5; i++ {
		if msg := readJSON(t, conn); msg["type"] == "sync" {
			syncIOS, _ = msg["ios"].(map[string]any)
			break
		}
	}
	if _, ok := syncIOS["ldr"]; !ok {
		t.Fatal("depois de virar switch, o IO deve voltar no sync")
	}

	h.mu.RLock()
	io := h.data.Devices["dev1"].IOs["ldr"]
	h.mu.RUnlock()
	if io.Kind != "switch" || io.Value != nil || io.ValueUpdatedAt != nil {
		t.Fatalf("valor deveria ser descartado ao virar switch: %+v", io)
	}
}

// O snapshot da API é encodado fora do RLock: o clone precisa copiar os
// ponteiros de valor, senão um report concorrente mistura value/timestamp.
func TestAPI_CloneNaoCompartilhaPonteiroDeValor(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	raw := json.RawMessage(`734`)
	now := time.Now().UTC()
	esperadoAt := now
	esperado := json.RawMessage(`734`)
	h.data.Devices["dev1"].IOs["ldr"] = &IOState{Name: "Luz", Kind: "number", Value: &raw, ValueUpdatedAt: &now}
	clone := cloneDevice(h.data.Devices["dev1"])
	// Simula quem reescreve o estado IN-PLACE entre o clone e o encode (o
	// UnmarshalJSON do gateway pode reusar o buffer do RawMessage). Se o clone
	// compartilha os ponteiros, ele passa a ver 999, não 734.
	ioVivo := h.data.Devices["dev1"].IOs["ldr"]
	*ioVivo.Value = json.RawMessage(`999`)
	*ioVivo.ValueUpdatedAt = now.Add(time.Second)
	h.mu.Unlock()

	if got := string(*clone.IOs["ldr"].Value); got != string(esperado) {
		t.Fatalf("clone deve capturar o valor do momento do clone, obtive %s", got)
	}
	if clone.IOs["ldr"].ValueUpdatedAt.IsZero() || *clone.IOs["ldr"].ValueUpdatedAt != esperadoAt {
		t.Fatalf("clone deve capturar o timestamp do momento do clone, obtive %v", clone.IOs["ldr"].ValueUpdatedAt)
	}
}

func TestReport_SemKindEhDescartado(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].IOs["ldr"] = &IOState{Name: "Luz", Kind: "number"}
	h.mu.Unlock()
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn)

	// Sem `kind` não há como saber se 734 é número ou texto: descarta.
	_ = conn.WriteJSON(map[string]any{"type": "report", "io": "ldr", "value": 734})
	esperaPong(t, conn)
	if v, ok := valorEm(h, "dev1", "ldr"); ok {
		t.Fatalf("report sem kind deveria ser descartado, gravou %s", *v)
	}
}

func TestReport_TextoAcimaDe4KBEhDescartado(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].IOs["st"] = &IOState{Name: "Status", Kind: "text"}
	h.mu.Unlock()
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn)

	reportar(t, conn, "st", strings.Repeat("x", 4097))
	esperaPong(t, conn)
	if v, ok := valorEm(h, "dev1", "st"); ok {
		t.Fatalf("texto acima de 4096 deveria ser descartado, gravou %d bytes", len(*v))
	}
}

func TestReport_EmiteSSESemPersistir(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	// Ponytail: campo Kind, não o map-literal — o literal bypassa MarshalJSON.
	h.data.Devices["dev1"].IOs["ldr"] = &IOState{Name: "Luz", Kind: "number"}
	h.mu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("/events", h.handleEvents)
	mux.HandleFunc("/ws", h.handleWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/events?api_key=" + testAPIKey)
	if err != nil {
		t.Fatalf("abre events: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	reader := bufio.NewReader(resp.Body)
	readEvent := func() string {
		t.Helper()
		var b strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("lê event: %v", err)
			}
			// keepalive do SSE (": keepalive") não é evento
			if strings.HasPrefix(line, ":") {
				continue
			}
			b.WriteString(line)
			if strings.TrimSpace(line) == "" && strings.Contains(b.String(), "data: ") {
				return b.String()
			}
		}
	}
	readEvent() // estado inicial

	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn)
	reportar(t, conn, "ldr", 512)

	deadline := time.Now().Add(2 * time.Second)
	for {
		ev := readEvent()
		if strings.Contains(ev, `"value":512`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("esperava SSE com value 512, obtive %q", ev)
		}
	}
}

func TestPersistencia_ValorDeIONaoVaiParaArquivo(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	raw := json.RawMessage(`734`)
	now := time.Now().UTC()
	h.data.Devices["dev1"].IOs["ldr"] = &IOState{Name: "Luz", Kind: "number", Value: &raw, ValueUpdatedAt: &now}
	h.mu.Unlock()

	// Qualquer save (renomear) grava o arquivo.
	srv := newTestServer(t, h)
	doRequest(t, srv, http.MethodPatch, "/api/devices/dev1", `{"name":"Sala"}`)

	gravado, _ := os.ReadFile(h.filePath)
	if strings.Contains(string(gravado), `"value": 734`) || strings.Contains(string(gravado), `"value":734`) {
		t.Fatalf("valor de IO não pode ir para o state.json:\n%s", gravado)
	}
	if !strings.Contains(string(gravado), `"kind": "number"`) {
		t.Fatalf("kind deve ser persistido:\n%s", gravado)
	}

	// E o arquivo carregado volta vazio, com kind preservado.
	h2, err := NewHub(h.filePath, testAPIKey)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	h2.mu.RLock()
	io := h2.data.Devices["dev1"].IOs["ldr"]
	h2.mu.RUnlock()
	if io.Kind != "number" || io.Value != nil {
		t.Fatalf("após restart: kind=%q value=%v (esperado number e vazio)", io.Kind, io.Value)
	}
}

func TestPersistencia_SaveDeAckNaoGravaValorEmMemoria(t *testing.T) {
	// Um save disparado por outro evento mantém o valor VIVO em memória.
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].IOs["ldr"] = &IOState{Name: "Luz", Kind: "number"}
	h.mu.Unlock()
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn)

	reportar(t, conn, "ldr", 42)
	_ = conn.WriteJSON(map[string]any{"type": "ack", "io": "gpio0", "state": true})
	esperaPong(t, conn)
	if v, ok := valorEm(h, "dev1", "ldr"); !ok || string(*v) != "42" {
		t.Fatalf("valor em memória deve sobreviver a um save de outro evento, obtive %v", v)
	}
}

// O painel precisa funcionar na raiz e em subpath (Caddy em /iot/): o <base
// href> acompanha o X-Forwarded-Prefix, e o JS usa document.baseURI em todas as
// chamadas em vez de "/api", "/events".
func TestUI_BaseHrefAcompanhaOPrefixoDoProxy(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.handleUI)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	get := func(prefix string) string {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
		if prefix != "" {
			req.Header.Set("X-Forwarded-Prefix", prefix)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get ui: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return string(raw)
	}

	raiz := get("")
	if !strings.Contains(raiz, `<base href="/">`) {
		t.Fatalf("na raiz o base href deve ser /")
	}
	if !strings.Contains(raiz, `<span id="build-version">`+buildVersion+`</span>`) {
		t.Fatalf("o painel deve exibir a versao do binario %q", buildVersion)
	}
	if strings.Contains(raiz, "{{BUILD_VERSION}}") {
		t.Fatal("o placeholder da versao nao deve ser servido sem substituicao")
	}

	sub := get("/iot")
	if !strings.Contains(sub, `<base href="/iot/">`) {
		t.Fatalf("em subpath o base href deve ser /iot/")
	}
	if strings.Contains(sub, `<base href="/iot">`) {
		t.Fatal("o base href precisa terminar com barra")
	}

	lixo := get("/iot\"><script>alert(1)</script>")
	if strings.Contains(lixo, "<script>alert(1)") {
		t.Fatal("prefixo com markup não pode ser injetado no HTML")
	}
}

// POST de IO em device existente continua criando o IO manualmente (§6, painel).
func TestAPI_PostCriaIONoPainel(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)

	status, body := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io",
		`{"io":"gpio4","state":true,"name":"Sirene"}`)
	if status != http.StatusAccepted {
		t.Fatalf("esperava 202 (offline), obtive %d %v", status, body)
	}
	h.mu.RLock()
	io := h.data.Devices["dev1"].IOs["gpio4"]
	h.mu.RUnlock()
	if io == nil || io.Name != "Sirene" || !io.State {
		t.Fatalf("IO deveria ser criado com nome e estado, obtive %+v", io)
	}
}

// O painel consome /events (SSE) e deve receber o estado atual na conexão e
// novamente assim que algo muda, sem precisar de polling (#UI).
func TestEvents_EnviaEstadoAtualEAposAlteracao(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	mux := http.NewServeMux()
	mux.HandleFunc("/events", h.handleEvents)
	mux.HandleFunc("/api/devices/", h.authMiddleware(h.handleDeviceByID))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/events?api_key="+testAPIKey, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("abre events: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("esperava 200, obtive %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type deveria ser text/event-stream, obtive %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	readEvent := func() string {
		t.Helper()
		var b strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("lê event: %v", err)
			}
			b.WriteString(line)
			if strings.TrimSpace(line) == "" && strings.Contains(b.String(), "data: ") {
				return b.String()
			}
		}
	}

	if ev := readEvent(); !strings.Contains(ev, "dev1") {
		t.Fatalf("primeiro event deveria trazer o estado atual com dev1, obtive %q", ev)
	}

	postIO(t, srv, "dev1", `{"io":"gpio0","state":true}`)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if ev := readEvent(); strings.Contains(ev, `"state":true`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("esperava event refletindo state=true")
		}
	}
}

// /events também exige a API Key.
func TestEvents_ExigeAPIKey(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/events", h.handleEvents)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("esperava 401 sem chave, obtive %d", resp.StatusCode)
	}
}

// O servidor nao deve inventar dispositivos fictícios no primeiro start (§2: MVP sem seed obrigatório).
func TestStart_EstadoInicialVazio(t *testing.T) {
	h := newTestHub(t)
	if len(h.data.Devices) != 0 {
		t.Fatalf("esperava estado inicial vazio, obtive %d dispositivos", len(h.data.Devices))
	}
}

// ping não deve reescrever o state.json a cada batida; last_seen é atualizado em
// memória e persiste no flush implícito de ack/disconnect (#2).
func TestPing_NaoReescreveArquivoDeEstado(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn) // sync

	rawBefore, err := os.ReadFile(h.filePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := conn.WriteJSON(map[string]any{"type": "ping"}); err != nil {
			t.Fatalf("envia ping: %v", err)
		}
		deadline := time.Now().Add(2 * time.Second)
		got := ""
		for got != "pong" && time.Now().Before(deadline) {
			_ = conn.SetReadDeadline(deadline)
			var msg map[string]any
			if err := conn.ReadJSON(&msg); err != nil {
				t.Fatalf("read pong: %v", err)
			}
			got, _ = msg["type"].(string)
		}
		if got != "pong" {
			t.Fatalf("esperava pong")
		}
	}

	rawAfter, err := os.ReadFile(h.filePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if string(rawAfter) != string(rawBefore) {
		t.Fatal("ping não deve reescrever o state.json")
	}
}

// Timestamps criados por um POST devem estar em UTC em memória (#3, §5). Rode com
// TZ não-UTC (ex.: TZ=America/Sao_Paulo) para distinguir time.Now() de .UTC().
func TestSetIO_UpdatedAtEmMemoriaDeveSerUTC(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)

	doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"gpio1","state":true}`)

	h.mu.RLock()
	updatedAt := h.data.Devices["dev1"].IOs["gpio1"].UpdatedAt
	createdAt := h.data.Devices["dev1"].PendingQueue[len(h.data.Devices["dev1"].PendingQueue)-1].CreatedAt
	h.mu.RUnlock()
	if _, offset := updatedAt.Zone(); offset != 0 {
		t.Fatalf("io.updated_at em memória deveria ser UTC, offset=%d (location=%v)", offset, updatedAt.Location())
	}
	if _, offset := createdAt.Zone(); offset != 0 {
		t.Fatalf("created_at em memória deveria ser UTC, offset=%d", offset)
	}
}

// Reenvio do mesmo estado não pode deixar um comando contraditório depois dele na
// fila, ou o ESP aplicaria o estado antigo por último (#4).
func TestFila_ReenvioDeEstadoRemoveContraditorios(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)

	doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"gpio0","state":true}`)
	doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"gpio0","state":false}`)
	doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"gpio0","state":true}`)

	h.mu.RLock()
	defer h.mu.RUnlock()
	queue := h.data.Devices["dev1"].PendingQueue
	if len(queue) != 1 {
		t.Fatalf("fila deveria conter apenas o último estado desejado, tem %d: %+v", len(queue), queue)
	}
	if !queue[0].State {
		t.Fatalf("último comando enfileirado deveria ser state=true, obtive %v", queue[0].State)
	}
}

// Um cliente registrado não pode ficar órfão quando o socket cai; o cliente deve
// sair do mapa de conexões ativas (#5).
func TestWS_SocketFechadoNaoDeixaClienteOrfao(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	readJSON(t, conn) // sync
	_ = conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.RLock()
		n := len(h.clients)
		h.mu.RUnlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cliente deveria ser removido após fechar o socket, restam %d", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// POST /api/devices/{id}/io com `value` (IOs number/text).
// ---------------------------------------------------------------------------

// registerIOParaValueTest cria o device e anuncia gpio2 (switch), st (text) e
// ldr (number), devolvendo a conexão WS já com o register processado.
func registerIOParaValueTest(t *testing.T) (*Hub, *httptest.Server, *websocket.Conn) {
	t.Helper()
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn) // sync inicial

	reg := map[string]any{
		"type": "register",
		"ios": map[string]any{
			"gpio2": map[string]any{"name": "LED", "kind": "switch", "state": false},
			"st":    map[string]any{"name": "Status", "kind": "text"},
			"ldr":   map[string]any{"name": "Luz", "kind": "number"},
		},
	}
	if err := conn.WriteJSON(reg); err != nil {
		t.Fatalf("envia register: %v", err)
	}
	_ = conn
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if msg := readJSON(t, conn); msg["type"] == "sync" {
			return h, srv, conn
		}
	}
	t.Fatal("esperava sync após register")
	return nil, nil, nil
}

func TestSetIOValueTextAceito(t *testing.T) {
	_, srv, _ := registerIOParaValueTest(t)

	status, out := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"st","value":"oi"}`)
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("esperava 200/202, obtive %d (%v)", status, out)
	}
	if out["value"] != "oi" {
		t.Fatalf("resposta deveria ecoar o valor enviado, obtive %v", out["value"])
	}
}

func TestSetIOValueNumberAceito(t *testing.T) {
	_, srv, _ := registerIOParaValueTest(t)

	status, out := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"ldr","value":42}`)
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("esperava 200/202, obtive %d (%v)", status, out)
	}
	if out["value"] != float64(42) {
		t.Fatalf("resposta deveria ecoar 42, obtive %v", out["value"])
	}
}

func TestSetIOStateEmIODeValorRejeitado(t *testing.T) {
	_, srv, _ := registerIOParaValueTest(t)

	status, _ := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"st","state":true}`)
	if status != http.StatusBadRequest {
		t.Fatalf("state em IO de valor deveria dar 400, obtive %d", status)
	}
}

func TestSetIOValueEmSwitchRejeitado(t *testing.T) {
	_, srv, _ := registerIOParaValueTest(t)

	status, _ := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"gpio2","value":"x"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("value em switch deveria dar 400, obtive %d", status)
	}
}

func TestSetIOValueSemIORejeitado(t *testing.T) {
	_, srv, _ := registerIOParaValueTest(t)

	status, _ := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"novo","value":"x"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("value em IO inexistente deveria dar 400, obtive %d", status)
	}
}

func TestSetIOStateEValueJuntosRejeitado(t *testing.T) {
	_, srv, _ := registerIOParaValueTest(t)

	status, _ := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"gpio2","state":true,"value":"x"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("state+value deveria dar 400, obtive %d", status)
	}
}

func TestSetIOValueTextoDe129BytesRejeitado(t *testing.T) {
	_, srv, _ := registerIOParaValueTest(t)
	body, _ := json.Marshal(map[string]any{"io": "st", "value": strings.Repeat("x", 129)})

	status, _ := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", string(body))
	if status != http.StatusBadRequest {
		t.Fatalf("texto de 129 bytes deveria dar 400, obtive %d", status)
	}
}

func TestSetIOValueTextoDe128BytesAceito(t *testing.T) {
	_, srv, _ := registerIOParaValueTest(t)
	body, _ := json.Marshal(map[string]any{"io": "st", "value": strings.Repeat("x", 128)})

	status, _ := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", string(body))
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("texto de 128 bytes deveria ser aceito, obtive %d", status)
	}
}

func TestSetIOValueTipoErradoRejeitado(t *testing.T) {
	_, srv, _ := registerIOParaValueTest(t)

	status, _ := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"st","value":1}`)
	if status != http.StatusBadRequest {
		t.Fatalf("número em IO text deveria dar 400, obtive %d", status)
	}
}

func TestSetIOStateFalseContinuaAceito(t *testing.T) {
	_, srv, _ := registerIOParaValueTest(t)

	status, resp := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"gpio2","state":false}`)
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("state:false deveria ser aceito, obtive %d (%v)", status, resp)
	}
	if _, hasValue := resp["value"]; hasValue {
		t.Fatal("resposta de switch não deveria ter value")
	}
}

func TestSetIOValueEntregaSetIOComValueNoWS(t *testing.T) {
	_, srv, conn := registerIOParaValueTest(t)

	status, _ := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"st","value":"oi"}`)
	if status != http.StatusOK {
		t.Fatalf("device online deveria entregar na hora (200), obtive %d", status)
	}

	msg := readJSON(t, conn)
	if msg["type"] != "set_io" || msg["io"] != "st" || msg["value"] != "oi" {
		t.Fatalf("esperava set_io com value \"oi\" para st, obtive %v", msg)
	}
	if _, hasState := msg["state"]; !hasState {
		t.Fatalf("state deve continuar presente por compatibilidade: %v", msg)
	}
}

// ---------------------------------------------------------------------------
// Fila de 1 para valor (offline) e drenagem no ack.
// ---------------------------------------------------------------------------

// Dois valores para o mesmo IO com o dispositivo offline: só o último fica na
// fila, coerente com a fila de 1 do take no ESP. Ambos respondem queued=true.
func TestSetIOValueOfflineSubstituiAnteriorNaFila(t *testing.T) {
	h, srv, _ := registerIOParaValueTest(t)
	// Fecha o canal do dispositivo para simular "offline" mantendo o IO
	// registrado; a fila é o caminho a testar.
	drainHub(t, h)

	statusA, outA := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"st","value":"a"}`)
	statusB, outB := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io", `{"io":"st","value":"b"}`)
	if statusA != http.StatusAccepted || statusB != http.StatusAccepted {
		t.Fatalf("offline deveria responder 202; a=%d b=%d", statusA, statusB)
	}
	if outA["queued"] != true || outB["queued"] != true {
		t.Fatalf("respostas deveriam ter queued=true: a=%v b=%v", outA["queued"], outB["queued"])
	}

	h.mu.RLock()
	queue := append([]PendingCommand(nil), h.data.Devices["dev1"].PendingQueue...)
	h.mu.RUnlock()
	if len(queue) != 1 {
		t.Fatalf("fila deveria ter 1 pendência (último valor), tem %d: %v", len(queue), queue)
	}
	if string(queue[0].Value) != `"b"` {
		t.Fatalf("a pendência deveria ser o último valor \"b\", obtive %s", queue[0].Value)
	}
}

// O ack de um comando de valor drena a pendência sem gravar estado nem valor:
// IO de valor não tem `state`, e o valor não persiste.
func TestAckDeValorDrenaSemGravarState(t *testing.T) {
	h, srv, _ := registerIOParaValueTest(t)
	h.mu.Lock()
	h.data.Devices["dev1"].PendingQueue = []PendingCommand{{
		ID: "cmd-v", Type: "set_io", IO: "st", Value: json.RawMessage(`"oi"`), CreatedAt: time.Now(),
	}}
	h.mu.Unlock()
	conn := connectWS(t, srv, "dev1")
	readJSON(t, conn) // sync
	msg := readSetIO(t, conn)
	if msg["value"] != "oi" {
		t.Fatalf("pendência de valor deveria ser entregue: %v", msg)
	}

	if err := conn.WriteJSON(map[string]any{"type": "ack", "io": "st", "state": false, "command_id": "cmd-v"}); err != nil {
		t.Fatalf("envia ack: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.RLock()
		queue := len(h.data.Devices["dev1"].PendingQueue)
		entry := h.data.Devices["dev1"].IOs["st"]
		state := entry.State
		updated := entry.UpdatedAt
		value := entry.Value
		h.mu.RUnlock()
		if queue == 0 {
			if state {
				t.Fatal("ack de valor não deveria gravar state")
			}
			if !updated.IsZero() {
				t.Fatalf("ack de valor não deveria tocar updated_at, obtive %v", updated)
			}
			if value != nil {
				t.Fatalf("ack de valor não deveria gravar value, obtive %s", *value)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ack deveria drenar a pendência de valor, fila=%d", queue)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Ack de valor espontâneo (sem command_id) num IO text/number não pode gravar
// estado: IO de valor não tem `state`. Cobre firmware divergente do gateway.
func TestAckEspontaneoEmIODeValorNaoGravaEstado(t *testing.T) {
	h, srv, _ := registerIOParaValueTest(t)
	conn := connectWS(t, srv, "dev1")
	readJSON(t, conn) // sync

	if err := conn.WriteJSON(map[string]any{"type": "ack", "io": "st", "state": true}); err != nil {
		t.Fatalf("envia ack: %v", err)
	}

	// Dá tempo para o handler processar e então confere o invariante.
	time.Sleep(150 * time.Millisecond)
	h.mu.RLock()
	entry := h.data.Devices["dev1"].IOs["st"]
	state := entry.State
	updated := entry.UpdatedAt
	h.mu.RUnlock()
	if state {
		t.Fatal("ack espontâneo em IO de valor não deveria gravar state")
	}
	if !updated.IsZero() {
		t.Fatalf("ack espontâneo em IO de valor não deveria tocar updated_at, obtive %v", updated)
	}
}

// Pendência de valor para IO que o firmware não tem: o ack (com io preenchido)
// precisa drenar a fila mesmo quando o IO não existe no register.
func TestAckDeValorParaIODesconhecidoDrenaFila(t *testing.T) {
	h, srv, _ := registerIOParaValueTest(t)
	h.mu.Lock()
	h.data.Devices["dev1"].PendingQueue = []PendingCommand{{
		ID: "cmd-f", Type: "set_io", IO: "fantasma", Value: json.RawMessage(`"x"`), CreatedAt: time.Now(),
	}}
	h.mu.Unlock()
	conn := connectWS(t, srv, "dev1")
	readJSON(t, conn) // sync
	readSetIO(t, conn)

	if err := conn.WriteJSON(map[string]any{"type": "ack", "io": "fantasma", "state": false, "command_id": "cmd-f"}); err != nil {
		t.Fatalf("envia ack: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if queueLen(h, "dev1") == 0 {
			h.mu.RLock()
			_, created := h.data.Devices["dev1"].IOs["fantasma"]
			h.mu.RUnlock()
			if created {
				t.Fatal("ack de valor não deveria criar IO")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("ack de valor para IO desconhecido deveria drenar a fila")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Regressão: ack de switch continua gravando estado e updated_at.
func TestAckDeSwitchContinuaGravandoEstado(t *testing.T) {
	h, srv, _ := registerIOParaValueTest(t)
	h.mu.Lock()
	h.data.Devices["dev1"].PendingQueue = []PendingCommand{{
		ID: "cmd-s", Type: "set_io", IO: "gpio2", State: false, CreatedAt: time.Now(),
	}}
	h.mu.Unlock()
	conn := connectWS(t, srv, "dev1")
	readJSON(t, conn) // sync
	readSetIO(t, conn)

	if err := conn.WriteJSON(map[string]any{"type": "ack", "io": "gpio2", "state": true, "command_id": "cmd-s"}); err != nil {
		t.Fatalf("envia ack: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.RLock()
		queue := len(h.data.Devices["dev1"].PendingQueue)
		entry := h.data.Devices["dev1"].IOs["gpio2"]
		state := entry.State
		updated := entry.UpdatedAt
		h.mu.RUnlock()
		if queue == 0 && state && !updated.IsZero() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ack de switch deveria gravar state=true e updated_at; state=%v updated=%v fila=%d",
				state, updated, queue)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Remoção de IO do console (DELETE .../io/{io}).
// ---------------------------------------------------------------------------

// IO desligado some do estado sem gerar comando: não havia nada a corrigir no ESP.
func TestDeleteIO_RemoveSemComando(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)

	status, out := doRequest(t, srv, http.MethodDelete, "/api/devices/dev1/io/gpio0", "")
	if status != http.StatusOK {
		t.Fatalf("esperava 200, obtive %d (%v)", status, out)
	}
	if out["io"] != "gpio0" || out["queued"] != false || out["delivered"] != false {
		t.Fatalf("resposta inesperada: %v", out)
	}

	h.mu.RLock()
	_, exists := h.data.Devices["dev1"].IOs["gpio0"]
	queue := len(h.data.Devices["dev1"].PendingQueue)
	h.mu.RUnlock()
	if exists {
		t.Fatal("IO removido deveria sair do estado em memória")
	}
	if queue != 0 {
		t.Fatalf("IO desligado não deveria gerar pendência, fila=%d", queue)
	}
}

// O IO removido não pode voltar pelo state.json depois de um restart.
func TestDeleteIO_NaoVoltaDoArquivo(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].IOs["ldr"] = &IOState{Name: "LDR", Kind: "number"}
	h.mu.Unlock()
	srv := newTestServer(t, h)

	if status, _ := doRequest(t, srv, http.MethodDelete, "/api/devices/dev1/io/ldr", ""); status != http.StatusOK {
		t.Fatalf("esperava 200, obtive %d", status)
	}

	raw, err := os.ReadFile(h.filePath)
	if err != nil {
		t.Fatalf("lê state.json: %v", err)
	}
	if strings.Contains(string(raw), `"ldr"`) {
		t.Fatalf("state.json não deveria conter o IO removido: %s", raw)
	}
}

// Um relé ligado que sai do painel precisa ser desligado no ESP, com a fila
// limpa antes (pendência do IO removido não sobrevive).
func TestDeleteIO_SwitchLigadoDesligaNoFirmware(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].IOs["gpio0"].State = true
	h.data.Devices["dev1"].PendingQueue = []PendingCommand{
		{ID: "cmd-antigo", Type: "set_io", IO: "gpio0", State: false, CreatedAt: time.Now()},
		{ID: "cmd-outro", Type: "set_io", IO: "gpio9", State: true, CreatedAt: time.Now()},
	}
	h.mu.Unlock()
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	readJSON(t, conn)  // sync
	readSetIO(t, conn) // cmd-outro
	readSetIO(t, conn) // cmd-antigo

	status, out := doRequest(t, srv, http.MethodDelete, "/api/devices/dev1/io/gpio0", "")
	// "delivered" é uma decisão do caminho online; aqui só importa o comando.
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("esperava 200/202, obtive %d (%v)", status, out)
	}

	msg := readJSON(t, conn)
	if msg["type"] != "set_io" || msg["io"] != "gpio0" || msg["state"] != false {
		t.Fatalf("esperava set_io state=false para gpio0, obtive %v", msg)
	}

	h.mu.RLock()
	_, exists := h.data.Devices["dev1"].IOs["gpio0"]
	queue := append([]PendingCommand(nil), h.data.Devices["dev1"].PendingQueue...)
	h.mu.RUnlock()
	if exists {
		t.Fatal("IO removido deveria sair do estado")
	}
	if len(queue) != 1 || queue[0].IO != "gpio9" {
		t.Fatalf("só a pendência do IO removido deveria sair; fila=%v", queue)
	}
}

// Offline: o desligamento do relé fica na fila e sai na reconexão.
func TestDeleteIO_OfflineEnfileiraDesligamento(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].IOs["gpio0"].State = true
	h.mu.Unlock()
	srv := newTestServer(t, h)

	status, out := doRequest(t, srv, http.MethodDelete, "/api/devices/dev1/io/gpio0", "")
	if status != http.StatusAccepted {
		t.Fatalf("offline deveria responder 202, obtive %d (%v)", status, out)
	}
	if out["queued"] != true {
		t.Fatalf("resposta deveria ter queued=true: %v", out)
	}

	h.mu.RLock()
	queue := append([]PendingCommand(nil), h.data.Devices["dev1"].PendingQueue...)
	h.mu.RUnlock()
	if len(queue) != 1 || queue[0].IO != "gpio0" || queue[0].State {
		t.Fatalf("esperava 1 pendência desligando gpio0, obtive %v", queue)
	}

	conn := connectWS(t, srv, "dev1")
	readJSON(t, conn) // sync
	if msg := readSetIO(t, conn); msg["io"] != "gpio0" || msg["state"] != false {
		t.Fatalf("reconexão deveria entregar set_io state=false, obtive %v", msg)
	}
}

// Ack do desligamento de um IO removido drena a pendência e não ressuscita o IO.
func TestDeleteIO_AckNaoRessuscitaIO(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].IOs["gpio0"].State = true
	h.mu.Unlock()
	srv := newTestServer(t, h)

	if status, _ := doRequest(t, srv, http.MethodDelete, "/api/devices/dev1/io/gpio0", ""); status != http.StatusAccepted {
		t.Fatalf("esperava 202 para dispositivo offline, obtive %d", status)
	}
	h.mu.RLock()
	cmdID := h.data.Devices["dev1"].PendingQueue[0].ID
	h.mu.RUnlock()

	conn := connectWS(t, srv, "dev1")
	readJSON(t, conn) // sync
	readSetIO(t, conn)
	if err := conn.WriteJSON(map[string]any{"type": "ack", "io": "gpio0", "state": false, "command_id": cmdID}); err != nil {
		t.Fatalf("envia ack: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.RLock()
		queue := len(h.data.Devices["dev1"].PendingQueue)
		_, exists := h.data.Devices["dev1"].IOs["gpio0"]
		h.mu.RUnlock()
		if queue == 0 {
			if exists {
				t.Fatal("ack de IO removido não deveria recriar o IO")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ack deveria drenar o desligamento, fila=%d", queue)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDeleteIO_InexistenteRetorna404(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)

	if status, _ := doRequest(t, srv, http.MethodDelete, "/api/devices/dev1/io/sumiu", ""); status != http.StatusNotFound {
		t.Fatalf("IO inexistente deveria dar 404, obtive %d", status)
	}
	if status, _ := doRequest(t, srv, http.MethodDelete, "/api/devices/nao-existe/io/gpio0", ""); status != http.StatusNotFound {
		t.Fatalf("dispositivo inexistente deveria dar 404, obtive %d", status)
	}
}

// POST sem sub-recurso continua no 405 com Allow, agora convivendo com o
// DELETE de IO no mesmo prefixo.
func TestDeleteIO_MetodoNaoSuportadoNoSubRecurso(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)

	status, _ := doRequest(t, srv, http.MethodPost, "/api/devices/dev1/io/gpio0", "")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("POST em /io/{io} deveria dar 405, obtive %d", status)
	}
}
