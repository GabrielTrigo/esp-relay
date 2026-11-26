#pragma once

// Copie este arquivo para src/secrets.h e preencha com seus dados.
// src/secrets.h NÃO é versionado.

// --- Wi-Fi ---
#define WIFI_SSID "SEU_WIFI"
#define WIFI_PASS "SUA_SENHA"

// --- Gateway Go ---
// Rede local: ESP fala direto com o gateway.
//   WS_HOST "192.168.1.10" / WS_PORT 8080 / USE_WSS false
// Proxy HTTPS (ex.: Caddy em https://www.exemplo.com/iot/):
//   use o DOMÍNIO (o certificado depende do hostname), a porta do proxy (443),
//   USE_WSS true. O caminho /ws é fixo e o proxy já remove o prefixo /iot.
#define WS_HOST "192.168.1.10"
#define WS_PORT 8080
#define DEVICE_ID "esp-relay-demo"
#define API_KEY "SUA_IOT_API_KEY"
#define USE_WSS false

// Prefixo do gateway atrás de proxy em subpath ("/iot" para
// www.exemplo.com/iot/); "" quando o gateway fica na raiz.
#define WS_PATH ""

// SHA-1 do certificado, ou nullptr para não validar (padrão). Let's Encrypt
// renova a cada 90 dias e muda o fingerprint: com valor fixo, o ESP para de
// conectar até regravar. Obter com:
//   openssl s_client -connect www.exemplo.com:443 -servername www.exemplo.com </dev/null 2>/dev/null | openssl x509 -fingerprint -sha1 -noout
// #define TLS_FINGERPRINT "AA:BB:CC:..."
