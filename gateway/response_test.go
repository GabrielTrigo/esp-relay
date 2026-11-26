package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// deadWSClient cria um *websocket.Conn já fechado localmente, de modo que
// WriteJSON falha imediatamente e de forma determinística.
func deadWSClient(t *testing.T) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Mantém a conexão aberta para o handshake completar.
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial dead ws: %v", err)
	}
	// Fecha o lado usado pelo servidor do Hub: qualquer escrita falha.
	_ = conn.Close()
	return conn
}

// "queued" na resposta deve refletir a decisão real de enfileirar, e não ser
// derivado de "delivered". Estado fixado: device online em memória, mas cliente
// WS com socket morto, de modo que o envio falha (delivered=false) enquanto o
// comando NÃO foi enfileirado (queued=false).
func TestResposta_QueuedRefleteDecisaoRealNaoDelivered(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")

	// Registra um cliente WS cujo socket está morto: escrita falha.
	conn := deadWSClient(t)
	client := &WSClient{deviceID: "dev1", conn: conn}

	h.mu.Lock()
	h.clients["dev1"] = client
	h.data.Devices["dev1"].Online = true
	h.mu.Unlock()

	srv := newTestServer(t, h)
	status, body := postIO(t, srv, "dev1", `{"io":"gpio0","state":true}`)

	if body["delivered"] == true {
		t.Fatalf("envio deveria ter falhado (delivered=false), resposta=%v", body)
	}
	if body["queued"] != false {
		t.Fatalf("queued deveria ser false (comando não foi enfileirado), resposta=%v", body)
	}
	if n := queueLen(h, "dev1"); n != 0 {
		t.Fatalf("comando não deveria estar na fila com device online; fila=%d", n)
	}
	if status != http.StatusAccepted {
		t.Fatalf("esperava 202 quando o envio falha, obtive %d", status)
	}
}

// Sanidade: com o ESP offline (sem socket), queued=true e delivered=false.
func TestResposta_QueuedTrueQuandoOffline(t *testing.T) {
	h := newTestHub(t)
	withTestDevice(h, "dev1")
	srv := newTestServer(t, h)

	status, body := postIO(t, srv, "dev1", `{"io":"gpio0","state":true}`)
	if status != http.StatusAccepted {
		t.Fatalf("esperava 202 offline, obtive %d", status)
	}
	if body["queued"] != true || body["delivered"] != false {
		t.Fatalf("esperava queued=true delivered=false, obtive %v", body)
	}
	if n := queueLen(h, "dev1"); n != 1 {
		t.Fatalf("esperava 1 item na fila, obtive %d", n)
	}
}
