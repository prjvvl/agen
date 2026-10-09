/* Agen C ABI. All strings are UTF-8 JSON unless noted. Strings returned by
 * the library must be released with agen_string_free. Callbacks may run on
 * any thread and receive borrowed strings valid only during the call. */
#ifndef AGEN_H
#define AGEN_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct AgenAgent AgenAgent;

/* request_json: {"id","kind":"tool","name","arguments","runId","callId"}
 *            or {"id","kind":"approval","tool","arguments","runId"}.
 * Answer with agen_complete(agent, id, result, is_error). */
typedef void (*agen_host_cb)(void *user_data, const char *request_json);

/* event_json: {"type":"delta","text"} | {"type":"reset"} | {"type":"done","result":{...}} */
typedef void (*agen_event_cb)(void *user_data, const char *event_json);

AgenAgent *agen_agent_new(const char *spec_json, agen_host_cb host_cb, void *user_data, char **err_out);
char *agen_run(const AgenAgent *agent, const char *input, const char *options_json,
               agen_event_cb event_cb, void *user_data, char **err_out);
int32_t agen_complete(const AgenAgent *agent, const char *request_id, const char *result, int32_t is_error);
int32_t agen_cancel(const AgenAgent *agent, const char *key);
/* Spans of a trace: [{"spanId","parentSpanId","name","runId","startMs","endMs","status","attributes"}] */
char *agen_trace(const AgenAgent *agent, const char *trace_id, char **err_out);
/* Reject new runs, cancel running ones, fail pending host requests. Call before agen_agent_free. */
void agen_shutdown(const AgenAgent *agent);
void agen_agent_free(AgenAgent *agent);
void agen_string_free(char *s);
const char *agen_version(void);

#ifdef __cplusplus
}
#endif

#endif
