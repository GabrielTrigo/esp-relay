#pragma once

// Copie este arquivo para src/secrets.h e preencha com seus dados.
// src/secrets.h NÃO é versionado (veja .gitignore).

// --- Wi-Fi ---
#define WIFI_SSID "SEU_WIFI"
#define WIFI_PASS "SUA_SENHA"

// --- Gateway Go ---
// Rede local: o ESP fala direto com o gateway, sem TLS.
//   #define WS_HOST "192.168.1.10"
//   #define WS_PORT 8080
//   #define USE_WSS false
//
// Servidor atrás de proxy HTTPS (ex.: Caddy em https://www.exemplo.com/iot/):
//   - use o DOMÍNIO, não o IP: o certificado e o SNI dependem do hostname;
//   - a porta é a do proxy (443), não a do gateway (8081, que fica no loopback);
//   - o caminho /ws é fixo; o proxy em /iot/* já o encaminha sem o prefixo.
//   #define WS_HOST "www.exemplo.com"
//   #define WS_PORT 443
//   #define USE_WSS true
//
// Se o ESP não conectar, confira primeiro se porta e USE_WSS combinam
// (443 exige true; 8080, false).
#define WS_HOST "www.exemplo.com"
#define WS_PORT 443
#define DEVICE_ID "esp-relay-demo"
#define API_KEY "SUA_IOT_API_KEY"
#define USE_WSS true

// Prefixo do gateway quando ele está atrás de um proxy em subpath.
// Caddy com `handle_path /iot/*` em www.exemplo.com  -> "/iot"
// Gateway na raiz (LAN ou domínio próprio)           -> ""
#define WS_PATH "/iot"

// Validação do certificado (só importa com USE_WSS true). Passado como
// `fingerprint` ao IotDevice:
//   nullptr  -> TLS cifrado SEM validar o servidor (padrão; serve para ESP8266).
//   "AA:BB:..." -> valida o SHA-1 do certificado. Let's Encrypt renova a cada
//                  90 dias e o fingerprint muda: o ESP para de conectar até
//                  você regravar. Só use se aceitar essa manutenção.
// Obter o SHA-1 (na sua máquina, não no ESP):
//   openssl s_client -connect www.exemplo.com:443 -servername www.exemplo.com </dev/null 2>/dev/null \
//     | openssl x509 -fingerprint -sha1 -noout
// No Linux, `echo |` também resolve o </dev/null do Windows.
// O main.cpp usa esta macro; sem ela aqui, o valor padrão é nullptr.
// #define TLS_FINGERPRINT "AA:BB:CC:..."
