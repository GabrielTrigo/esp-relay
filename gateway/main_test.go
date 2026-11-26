package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testAPIKey = "test-key"

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	// O arquivo fica em t.TempDir() e o cleanup roda antes de o Go remover o
	// diretório. Handlers ainda em voo escrevem o state.json durante o teste
	// (WS recém-fechado, SSE), e no Windows um RemoveAll que pega o arquivo aberto
	// injeta ".tmp" no meio do caminho remoto, deixando o TempDir sujo.
	dir := t.TempDir()
	h, err := NewHub(filepath.Join(dir, "state.json"), testAPIKey)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	t.Cleanup(func() { waitStateWritesIdle(h) })
	return h
}

// waitStateWritesIdle espera o state.json parar de ser escrito. Sem isso o
// RemoveAll do TempDir pode pegar o arquivo aberto no meio de um save de
// handler que já terminou de rodar (WS recém-desconectado, SSE) — no Windows
// isso injeta ".tmp" no caminho remoto e "o diretório não está vazio" reprova
// o teste mesmo quando o comportamento está certo.
func waitStateWritesIdle(h *Hub) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.RLock()
		start := h.writeSeq
		h.mu.RUnlock()
		if start%2 == 0 {
			done := make(chan struct{})
			go func() {
				h.inflight.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
			h.mu.RLock()
			steady := h.writeSeq == start
			h.mu.RUnlock()
			if steady {
				return
			}
			continue
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// drainHub encerra as conexões e espera o handler de cada uma sair de h.clients,
// que é o último ponto em que ele grava o state.json (o write roda fora do lock).
func drainHub(t *testing.T, h *Hub) {
	t.Helper()
	h.mu.Lock()
	clients := make([]*WSClient, 0, len(h.clients))
	for _, c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		c.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.RLock()
		n := len(h.clients)
		h.mu.RUnlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newTestServer(t *testing.T, h *Hub) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", h.handleHealth)
	mux.HandleFunc("/api/devices", h.authMiddleware(h.handleListDevices))
	mux.HandleFunc("/api/devices/", h.authMiddleware(h.handleDeviceByID))
	mux.HandleFunc("/ws", h.handleWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// connectWS abre um socket de dispositivo no servidor de teste.
func connectWS(t *testing.T, srv *httptest.Server, deviceID string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?device_id=" + deviceID + "&api_key=" + testAPIKey
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readJSON(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msg map[string]any
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read ws message: %v", err)
	}
	return msg
}

func readSetIO(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	for {
		msg := readJSON(t, conn)
		if msg["type"] == "set_io" {
			return msg
		}
	}
}

func postIO(t *testing.T, srv *httptest.Server, deviceID, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/devices/"+deviceID+"/io", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-API-Key", testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post io: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func withTestDevice(h *Hub, deviceID string) {
	now := time.Now()
	h.mu.Lock()
	h.data.Devices[deviceID] = &DeviceState{
		ID:           deviceID,
		Name:         deviceID,
		Online:       false,
		LastSeen:     now,
		IOs:          map[string]*IOState{"gpio0": {Name: "Relé", State: false, UpdatedAt: now}},
		PendingQueue: []PendingCommand{},
	}
	h.mu.Unlock()
}

// isSwitchKind espelha a regra do servidor: kind vazio/desconhecido é switch.
func isSwitchKind(kind string) bool {
	return kind == "" || kind == "switch"
}

func queueLen(h *Hub, deviceID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.data.Devices[deviceID].PendingQueue)
}

// Um comando entregue com o ESP online não deve permanecer na fila aguardando acks
// que o painel nunca envia; caso contrário comandos obsoletos são reenviados no reconnect.
func TestInterruptor_ComandoEntregueNaoFicaPresoNaFila(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn) // sync

	status, body := postIO(t, srv, "dev1", `{"io":"gpio0","state":true}`)
	if status != http.StatusOK || body["delivered"] != true {
		t.Fatalf("esperava 200 delivered=true, obtive %d %v", status, body["delivered"])
	}
	readSetIO(t, conn)

	if n := queueLen(h, "dev1"); n != 0 {
		t.Fatalf("fila deveria estar vazia após entrega online, tem %d itens", n)
	}
}

// Um ESP que NÃO envia ack (firmware atual) deve ser sincronizado sem que o toggle
// seguinte reenvie o estado anterior.
func TestInterruptor_TogglesConsecutivosSemAckNaoDeixamLixo(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn) // sync

	postIO(t, srv, "dev1", `{"io":"gpio0","state":true}`)
	postIO(t, srv, "dev1", `{"io":"gpio0","state":false}`)
	postIO(t, srv, "dev1", `{"io":"gpio0","state":true}`)

	// O servidor deve ter entregado set_io:true, set_io:false, set_io:true em ordem.
	want := []bool{true, false, true}
	for i, expected := range want {
		msg := readSetIO(t, conn)
		if msg["state"] != expected {
			t.Fatalf("set_io #%d: esperava state=%v, obtive %v", i+1, expected, msg["state"])
		}
	}
	if n := queueLen(h, "dev1"); n != 0 {
		t.Fatalf("fila deveria estar vazia após entregas online, tem %d itens", n)
	}
}

// Comando pendente entregue no sync (ESP reconectando) deve sair da fila após o ack.
func TestFila_PendenteSaiDaFilaNoAck(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].PendingQueue = []PendingCommand{{
		ID: "cmd-seed", Type: "set_io", IO: "gpio0", State: true, CreatedAt: time.Now(),
	}}
	h.mu.Unlock()
	srv := newTestServer(t, h)

	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn) // sync
	msg := readSetIO(t, conn)
	if msg["command_id"] != "cmd-seed" {
		t.Fatalf("esperava drenar cmd-seed, obtive %v", msg["command_id"])
	}

	if err := conn.WriteJSON(map[string]any{"type": "ack", "command_id": "cmd-seed", "io": "gpio0", "state": true}); err != nil {
		t.Fatalf("envia ack: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for queueLen(h, "dev1") != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("fila não foi drenada após ack (tem %d itens)", queueLen(h, "dev1"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Um ack sem "io" ainda deve atualizar o estado efetivo e limpar a fila.
func TestFila_AckSemIOClearFilaELastSeenSemQuebrarEstado(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	h.mu.Lock()
	h.data.Devices["dev1"].PendingQueue = []PendingCommand{{
		ID: "cmd-x", Type: "set_io", IO: "gpio0", State: true, CreatedAt: time.Now(),
	}}
	h.mu.Unlock()
	srv := newTestServer(t, h)
	conn := connectWS(t, srv, "dev1")
	t.Cleanup(func() { drainHub(t, h) })
	readJSON(t, conn)
	readSetIO(t, conn)

	if err := conn.WriteJSON(map[string]any{"type": "ack", "command_id": "cmd-x"}); err != nil {
		t.Fatalf("envia ack: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for queueLen(h, "dev1") != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("fila não foi drenada após ack sem io")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
