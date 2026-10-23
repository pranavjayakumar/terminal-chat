#ifndef TC_VPIO_H
#define TC_VPIO_H

#include <stdint.h>

typedef struct tc_vpio tc_vpio;

// tc_vpio_open starts Apple's voice-processing I/O unit (echo cancellation,
// noise suppression, AGC if agc) at 48 kHz mono int16 on the default
// devices. handle is passed back to tcVPIOCallback. Returns NULL and sets *status
// on failure.
tc_vpio *tc_vpio_open(uintptr_t handle, int agc, int32_t *status);
void tc_vpio_close(tc_vpio *v);

#endif
