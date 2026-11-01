// C side of the JNI bridge between com.awgpilot.app.Core and the Go core.
#include <jni.h>
#include <stdint.h>

// Strings cross the boundary as UTF-16 so emoji in profile names survive.
uint16_t *awg_jstr(JNIEnv *env, jstring s, int *len);
void awg_free(void *p);
jstring awg_newstr(JNIEnv *env, const uint16_t *s, int len);

// Calls into Core's static methods from any (Go) thread.
uint16_t *awg_establish(const uint16_t *plan, int planLen, int *outLen);
void awg_release(void);
// Writes one line to logcat (tag AWGPilot).
void awg_log(const char *line);
void awg_notify(const uint16_t *title, int titleLen, const uint16_t *body, int bodyLen);
// Package name of the app owning a tunnel connection (empty when unknown).
uint16_t *awg_owner(int proto, const uint16_t *local, int localLen, int lport, const uint16_t *remote, int remoteLen, int rport, int *outLen);
