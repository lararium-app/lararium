package app.lararium.data

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONObject

/** Minimal hearthd v1 client (raw Net layer; OkHttp may return in slice 2). */
class HearthClient(private val ep: Endpoint) {

    data class Health(val ok: Boolean, val version: String, val model: String?)

    suspend fun health(): Result<Health> = withContext(Dispatchers.IO) {
        runCatching {
            val resp = Net.get(
                "$baseUrl/v1/health",
                headers = mapOf("Authorization" to "Bearer ${ep.token}")
            )
            if (resp.status == 401 || resp.status == 403) error("token refused")
            if (resp.status != 200) error("http ${resp.status}")
            val j = JSONObject(resp.body)
            Health(
                ok = j.optBoolean("ok", false),
                version = j.optString("version", "?"),
                model = j.optString("model").ifBlank { null }
            )
        }
    }

    private val baseUrl get() = ep.baseUrl.trimEnd('/')
}
