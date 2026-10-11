package app.lararium.data

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL

/** Minimal hearthd v1 client (stdlib only; OkHttp comes in slice 2 with SSE). */
class HearthClient(private val ep: Endpoint) {

    data class Health(val ok: Boolean, val version: String, val model: String?)

    suspend fun health(): Result<Health> = withContext(Dispatchers.IO) {
        runCatching {
            val conn = (URL("$baseUrl/v1/health").openConnection() as HttpURLConnection).apply {
                requestMethod = "GET"
                setRequestProperty("Authorization", "Bearer ${ep.token}")
                connectTimeout = 5_000
                readTimeout = 5_000
            }
            val code = conn.responseCode
            if (code == 401 || code == 403) error("token refused")
            if (code != 200) error("http $code")
            val j = JSONObject(conn.inputStream.bufferedReader().readText())
            Health(
                ok = j.optBoolean("ok", false),
                version = j.optString("version", "?"),
                model = j.optString("model").ifBlank { null }
            )
        }
    }

    private val baseUrl get() = ep.baseUrl.trimEnd('/')
}
