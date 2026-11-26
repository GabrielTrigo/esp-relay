// go:build ui

// Teste de regressão do painel em navegador real (Chrome via CDP).
// Requer: CHROME_PATH e um servidor rodando em BASE_URL, ex.:
//
//	go test -tags ui -run TestUISwitchNaoRecriaCard -v
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type cdpTarget struct {
	WebSocketDebuggerUrl string `json:"webSocketDebuggerUrl"`
}

type cdp struct {
	ws *websocket.Conn
	id int
}

func dialCDP(t *testing.T) *cdp {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1:9222/json/new", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("Chrome DevTools indisponível: %v", err)
	}
	defer resp.Body.Close()
	var target cdpTarget
	if err := json.NewDecoder(resp.Body).Decode(&target); err != nil {
		t.Fatalf("decode target: %v", err)
	}
	ws, _, err := websocket.DefaultDialer.Dial(target.WebSocketDebuggerUrl, nil)
	if err != nil {
		t.Fatalf("dial cdp: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return &cdp{ws: ws}
}

func (c *cdp) call(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	c.id++
	msg := map[string]any{"id": c.id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := c.ws.WriteJSON(msg); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
	for {
		_ = c.ws.SetReadDeadline(time.Now().Add(20 * time.Second))
		_, b, err := c.ws.ReadMessage()
		if err != nil {
			t.Fatalf("read %s: %v", method, err)
		}
		var r map[string]any
		_ = json.Unmarshal(b, &r)
		if rid, ok := r["id"].(float64); ok && int(rid) == c.id {
			return r
		}
	}
}

func (c *cdp) eval(t *testing.T, expr string) string {
	t.Helper()
	r := c.call(t, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true,
	})
	res, _ := r["result"].(map[string]any)
	v, _ := res["result"].(map[string]any)
	s, _ := v["value"].(string)
	return s
}

// dialUIPage exige um painel rodando em BASE_URL (ou 18097) e devolve a página
// aberta. Sem servidor (ou sem Chrome em :9222) o teste é pulado: são testes
// de integração com setup manual, não parte de `go test ./...` do dia a dia.
func dialUIPage(t *testing.T) *cdp {
	t.Helper()
	base := os.Getenv("BASE_URL")
	if base == "" {
		base = "http://127.0.0.1:18097"
	}
	key := os.Getenv("UI_KEY")
	if resp, err := http.Get(base + "/health"); err != nil {
		t.Skipf("painel indisponível em %s: %v", base, err)
	} else {
		resp.Body.Close()
	}

	c := dialCDP(t)
	c.call(t, "Page.enable", nil)
	c.call(t, "Runtime.enable", nil)
	c.call(t, "Page.navigate", map[string]any{"url": base + "/#key=" + key})
	time.Sleep(4 * time.Second)
	return c
}

// IO de valor (`number`/`text`) mostra o último report em vez do toggle, e um
// report novo atualiza o texto sem recriar o card (só há SSE, sem re-render de
// página a cada amostra).
func TestUIValueSemRecriarCard(t *testing.T) {
	c := dialUIPage(t)
	_ = c

	if got := c.eval(t, `String(!!document.querySelector('[data-value-io="ldr"]'))`); got != "true" {
		t.Fatalf("esperava linha de valor para ldr, obtive %s", got)
	}
	if got := c.eval(t, `String(!!document.querySelector('[data-toggle="ldr"]'))`); got != "false" {
		t.Fatal("IO de valor não pode ter toggle")
	}

	c.eval(t, `
		window.__orig = document.querySelector('[data-device]');
		window.__recreated = 0;
		const mo = new MutationObserver(() => {
			const c = document.querySelector('[data-device]');
			if (c && c !== window.__orig) { window.__recreated++; window.__orig = c; }
		});
		mo.observe(document.getElementById('devices'), { childList: true });
		'ok'`)

	// O ambiente de UI precisa de um dispositivo reportando "ldr" durante
	// estes segundos: é o mesmo setup manual que o teste de switch exige.
	time.Sleep(3 * time.Second)

	if got := c.eval(t, `String(window.__recreated)`); got != "0" {
		t.Fatalf("report recriou o card %s vez(es)", got)
	}
	if v := c.eval(t, `document.querySelector('[data-value-io="ldr"]').textContent.trim()`); v == "" || v == "—" {
		t.Fatalf("valor do ldr deveria ter chegado, obtive %q", v)
	}

	// Regressão: o valor precisa CONTINUAR acompanhando os reports. Com o
	// `last_seen` fora do `liveSig`, o caminho "só o valor mudou" nunca era
	// reconhecido, o card era reescrito com o valor antigo e congelava até um
	// ping/switch mudar algo visível — parecia que só a primeira leitura valia.
	first := c.eval(t, `window.__ldr0 = document.querySelector('[data-value-io="ldr"]').textContent; window.__ldr0`)
	time.Sleep(3 * time.Second)
	last := c.eval(t, `document.querySelector('[data-value-io="ldr"]').textContent`)
	if first == last {
		t.Fatalf("valor do ldr não acompanhou os reports (parado em %q)", first)
	}
}

func TestUISwitchNaoRecriaCard(t *testing.T) {
	c := dialUIPage(t)

	if got := c.eval(t, `String(document.querySelectorAll('[data-device]').length)`); got != "1" {
		t.Fatalf("esperava 1 card, obtive %s", got)
	}

	// Observa recriação do nó do card e re-execução da animação de entrada.
	c.eval(t, `
		window.__orig = document.querySelector('[data-device]');
		window.__recreated = 0;
		window.__anim = 0;
		window.__orig.addEventListener('animationstart', (e) => {
			if (e.animationName === 'fade-up') window.__anim++;
		});
		const mo = new MutationObserver(() => {
			const c = document.querySelector('[data-device]');
			if (c && c !== window.__orig) { window.__recreated++; window.__orig = c; }
		});
		mo.observe(document.getElementById('devices'), { childList: true });
		'ok'`)

	c.eval(t, `document.querySelector('[data-toggle]').click(); 'clicked'`)
	time.Sleep(1800 * time.Millisecond)

	recreated := c.eval(t, `String(window.__recreated)`)
	anim := c.eval(t, `String(window.__anim)`)
	flash := c.eval(t, `String(document.querySelector('[data-device]').className.includes('animate-flash'))`)

	fmt.Printf("recriados=%s animacoes=%s flash=%s\n", recreated, anim, flash)
	if recreated != "0" {
		t.Fatalf("clicar no switch recriou o card %s vez(es); deve atualizar sem recriar", recreated)
	}
	if anim != "0" {
		t.Fatalf("animação fade-up re-executou %s vez(es) no card existente", anim)
	}
	if flash != "false" {
		t.Fatal("card não deveria levar animate-flash (sobrescreve o fundo)")
	}
}

// Regressão: enquanto o usuário digita no "adicionar IO", as atualizações de
// `last_seen` (a cada ping) não podem fechar o <details> nem apagar o texto. O
// cenário antes do fix: todo ping reescrevia o card e o formulário sumia.
func TestUIPingNaoDerrubaRascunhoDeIO(t *testing.T) {
	c := dialUIPage(t)

	if got := c.eval(t, `String(!!document.querySelector('details'))`); got != "true" {
		t.Fatal("esperava um card com o formulário de adicionar IO")
	}

	// Abre o <details> e digita como o usuário faria.
	c.eval(t, `
		const det = document.querySelector('details');
		det.open = true;
		const form = document.querySelector('[data-addio]');
		const io = form.elements.io;
		io.focus();
		io.value = 'gpio12';
		form.elements.name.value = 'Luz';
		'ok'`)

	// Um ping chega (last_seen muda) sem mudar mais nada: o card não deve ser reescrito.
	c.eval(t, `
		window.__det = document.querySelector('details');
		window.__io = document.querySelector('[data-addio]').elements.io;
		window.__rewrites = 0;
		const mo = new MutationObserver(() => {
			if (document.querySelector('details') !== window.__det) window.__rewrites++;
		});
		mo.observe(document.getElementById('devices'), { childList: true, subtree: true });
		'ok'`)

	time.Sleep(6 * time.Second) // pelo menos um ping/last_seen novo + ciclo do timer de 5s

	if got := c.eval(t, `String(window.__rewrites)`); got != "0" {
		t.Fatalf("ping reescreveu o card %s vez(es); o rascunho não pode ser destruído", got)
	}
	if got := c.eval(t, `String(document.querySelector('details').open)`); got != "true" {
		t.Fatal("<details> de adicionar IO fechou sozinho")
	}
	if got := c.eval(t, `document.querySelector('[data-addio]').elements.io.value`); got != "gpio12" {
		t.Fatalf("texto digitado sumiu, obtive %q", got)
	}
	if got := c.eval(t, `document.querySelector('[data-addio]').elements.name.value`); got != "Luz" {
		t.Fatalf("nome digitado sumiu, obtive %q", got)
	}
	if got := c.eval(t, `String(document.activeElement === window.__io)`); got != "true" {
		t.Fatal("foco saiu do campo enquanto o usuário digitava")
	}
}

// Regressão: um clique no resumo do "adicionar IO" abre e deixa aberto. O
// padrão nativo do <details> alterna no click, depois do pointerdown — um
// handler de pointerdown que só alternava fazia o painel abrir e fechar no
// mesmo clique. O clique é o de verdade (mousePressed/mouseReleased via CDP).
func TestUIAdicionarIOAbreComUmClique(t *testing.T) {
	c := dialUIPage(t)

	if got := c.eval(t, `String(!!document.querySelector('details summary'))`); got != "true" {
		t.Fatal("esperava o resumo do adicionar IO")
	}
	if got := c.eval(t, `String(document.querySelector('details').open)`); got != "false" {
		t.Fatal("o formulário deveria começar fechado")
	}

	r := c.call(t, "Runtime.evaluate", map[string]any{
		"expression": `(() => {
			const r = document.querySelector('details summary').getBoundingClientRect();
			return JSON.stringify({ x: r.x + r.width / 2, y: r.y + r.height / 2 });
		})()`,
		"returnByValue": true,
	})
	res, _ := r["result"].(map[string]any)
	val, _ := res["result"].(map[string]any)
	raw, _ := val["value"].(string)
	var pos struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	if err := json.Unmarshal([]byte(raw), &pos); err != nil {
		t.Fatalf("posição do resumo: %v (%q)", err, raw)
	}

	click := func(kind string) {
		c.call(t, "Input.dispatchMouseEvent", map[string]any{
			"type": kind, "x": pos.X, "y": pos.Y, "button": "left", "clickCount": 1,
		})
	}
	click("mousePressed")
	click("mouseReleased")
	time.Sleep(300 * time.Millisecond)

	if got := c.eval(t, `String(document.querySelector('details').open)`); got != "true" {
		t.Fatal("um clique no 'adicionar IO' deveria abrir e manter aberto")
	}
	if got := c.eval(t, `String(!!document.querySelector('[data-addio]').offsetParent)`); got != "true" {
		t.Fatal("o formulário deveria estar visível depois do clique")
	}

	// O segundo clique fecha.
	click("mousePressed")
	click("mouseReleased")
	time.Sleep(300 * time.Millisecond)
	if got := c.eval(t, `String(document.querySelector('details').open)`); got != "false" {
		t.Fatal("segundo clique deveria fechar o 'adicionar IO'")
	}
}
