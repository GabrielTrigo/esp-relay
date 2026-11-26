#pragma once

#include <stdint.h>

// Comparação segura contra o rollover de millis() (~49 dias). Comparar
// `millis() >= marca + intervalo` falha quando a soma estoura o uint32.
inline bool iotElapsed(uint32_t now, uint32_t mark, uint32_t interval) {
  return (uint32_t)(now - mark) >= interval;
}
