/*  liino.h - C ABI of the Liino native audio backend.
 *
 *  Built from the Go package github.com/team-quaver/liino/capi with:
 *      go build -buildmode=c-shared -tags liino_capi -o libliino.so .
 *
 *  The audio pipeline itself (decode, PCM, DSP, WASAPI/CoreAudio/PipeWire) is
 *  pure Go. Only this export layer needs the toolchain's C linker, and it is
 *  behind a build tag so the library builds and tests with CGO_ENABLED=0.
 *
 *  Conventions
 *  -----------
 *  - Every call returns 0 on success. Negative values are errors:
 *      -1 invalid argument   -2 not ready   -3 closed   -4 internal
 *  - When a call fails and `err` is non-NULL, *err receives a malloc'd message
 *    that the caller must release with liino_free_string().
 *  - Strings returned by liino_json_* / liino_next_event / liino_wait_event are
 *    malloc'd and must be released with liino_free_string().
 *  - JSON payloads are UTF-8, and their schema is documented in node/API.md.
 */
#ifndef LIINO_H
#define LIINO_H

#include <stdint.h>
#include <stdlib.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct liino_player liino_player;

/* Library identity and environment probe. */
const char *liino_version(void);
int liino_diagnostics(char **out_json);
int liino_devices(char **out_json);

/* Lifecycle. */
liino_player *liino_create(const char *config_json, char **err);
void liino_destroy(liino_player *player);

/* Transport. */
int liino_open(liino_player *player, const char *uri, const char *options_json, char **err);
int liino_play(liino_player *player, char **err);
int liino_pause(liino_player *player, char **err);
int liino_stop(liino_player *player, char **err);
int liino_seek_ms(liino_player *player, int64_t position_ms, char **err);

/* Mixer. */
int liino_set_volume(liino_player *player, double volume, char **err);
int liino_set_muted(liino_player *player, int muted, char **err);
int liino_set_eq(liino_player *player, const char *bands_json, char **err);
int liino_set_limiter(liino_player *player, int enabled, char **err);
int liino_set_bypass(liino_player *player, int bypass, char **err);

/* Routing and status. */
int liino_set_device(liino_player *player, const char *device_id, char **err);
int liino_snapshot_json(liino_player *player, char **out_json);
int liino_level_json(liino_player *player, char **out_json);

/* Events: poll without blocking, or wait up to timeout_ms. */
int liino_next_event(liino_player *player, char **out_json);
int liino_wait_event(liino_player *player, int timeout_ms, char **out_json);

/* Memory. */
void liino_free_string(char *ptr);

#ifdef __cplusplus
}
#endif

#endif /* LIINO_H */
