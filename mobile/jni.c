#include "awgjni.h"
#include <stdlib.h>
#include <string.h>
#include <android/log.h>

static JavaVM *g_vm;
static jclass g_core;
static jmethodID g_establish, g_release, g_notify, g_owner;

JNIEXPORT jint JNI_OnLoad(JavaVM *vm, void *reserved) {
	JNIEnv *env;
	g_vm = vm;
	if ((*vm)->GetEnv(vm, (void **)&env, JNI_VERSION_1_6) != JNI_OK)
		return JNI_ERR;
	jclass c = (*env)->FindClass(env, "com/awgpilot/app/Core");
	if (c == NULL)
		return JNI_ERR;
	g_core = (*env)->NewGlobalRef(env, c);
	g_establish = (*env)->GetStaticMethodID(env, g_core, "establish", "(Ljava/lang/String;)Ljava/lang/String;");
	g_release = (*env)->GetStaticMethodID(env, g_core, "release", "()V");
	g_notify = (*env)->GetStaticMethodID(env, g_core, "notify", "(Ljava/lang/String;Ljava/lang/String;)V");
	g_owner = (*env)->GetStaticMethodID(env, g_core, "owner", "(ILjava/lang/String;ILjava/lang/String;I)Ljava/lang/String;");
	if (g_establish == NULL || g_release == NULL || g_notify == NULL)
		return JNI_ERR;
	return JNI_VERSION_1_6;
}

void awg_log(const char *line) { __android_log_write(ANDROID_LOG_INFO, "AWGPilot", line); }

uint16_t *awg_jstr(JNIEnv *env, jstring s, int *len) {
	*len = 0;
	if (s == NULL)
		return NULL;
	jsize n = (*env)->GetStringLength(env, s);
	uint16_t *out = malloc((n + 1) * sizeof(uint16_t));
	(*env)->GetStringRegion(env, s, 0, n, (jchar *)out);
	*len = n;
	return out;
}

void awg_free(void *p) { free(p); }

jstring awg_newstr(JNIEnv *env, const uint16_t *s, int len) {
	return (*env)->NewString(env, (const jchar *)s, len);
}

// attach returns an env for the calling thread; *attached tells whether to detach afterwards.
static JNIEnv *attach(int *attached) {
	JNIEnv *env = NULL;
	*attached = 0;
	if ((*g_vm)->GetEnv(g_vm, (void **)&env, JNI_VERSION_1_6) == JNI_EDETACHED) {
		if ((*g_vm)->AttachCurrentThread(g_vm, &env, NULL) != JNI_OK)
			return NULL;
		*attached = 1;
	}
	return env;
}

static void detach(int attached) {
	if (attached)
		(*g_vm)->DetachCurrentThread(g_vm);
}

uint16_t *awg_establish(const uint16_t *plan, int planLen, int *outLen) {
	int attached;
	uint16_t *out = NULL;
	*outLen = 0;
	JNIEnv *env = attach(&attached);
	if (env == NULL)
		return NULL;
	jstring jplan = (*env)->NewString(env, (const jchar *)plan, planLen);
	jstring res = (jstring)(*env)->CallStaticObjectMethod(env, g_core, g_establish, jplan);
	if ((*env)->ExceptionCheck(env)) {
		(*env)->ExceptionClear(env);
		res = NULL;
	}
	if (res != NULL) {
		out = awg_jstr(env, res, outLen);
		(*env)->DeleteLocalRef(env, res);
	}
	(*env)->DeleteLocalRef(env, jplan);
	detach(attached);
	return out;
}

void awg_release(void) {
	int attached;
	JNIEnv *env = attach(&attached);
	if (env == NULL)
		return;
	(*env)->CallStaticVoidMethod(env, g_core, g_release);
	if ((*env)->ExceptionCheck(env))
		(*env)->ExceptionClear(env);
	detach(attached);
}

void awg_notify(const uint16_t *title, int titleLen, const uint16_t *body, int bodyLen) {
	int attached;
	JNIEnv *env = attach(&attached);
	if (env == NULL)
		return;
	jstring t = (*env)->NewString(env, (const jchar *)title, titleLen);
	jstring b = (*env)->NewString(env, (const jchar *)body, bodyLen);
	(*env)->CallStaticVoidMethod(env, g_core, g_notify, t, b);
	if ((*env)->ExceptionCheck(env))
		(*env)->ExceptionClear(env);
	(*env)->DeleteLocalRef(env, t);
	(*env)->DeleteLocalRef(env, b);
	detach(attached);
}

uint16_t *awg_owner(int proto, const uint16_t *local, int localLen, int lport, const uint16_t *remote, int remoteLen, int rport, int *outLen) {
	int attached;
	uint16_t *out = NULL;
	*outLen = 0;
	if (g_owner == NULL)
		return NULL;
	JNIEnv *env = attach(&attached);
	if (env == NULL)
		return NULL;
	jstring l = (*env)->NewString(env, (const jchar *)local, localLen);
	jstring r = (*env)->NewString(env, (const jchar *)remote, remoteLen);
	jstring res = (jstring)(*env)->CallStaticObjectMethod(env, g_core, g_owner, proto, l, lport, r, rport);
	if ((*env)->ExceptionCheck(env)) {
		(*env)->ExceptionClear(env);
		res = NULL;
	}
	if (res != NULL) {
		out = awg_jstr(env, res, outLen);
		(*env)->DeleteLocalRef(env, res);
	}
	(*env)->DeleteLocalRef(env, l);
	(*env)->DeleteLocalRef(env, r);
	detach(attached);
	return out;
}
