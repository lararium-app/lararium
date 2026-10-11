package app.lararium

import android.content.Context
import app.lararium.data.Endpoint
import kotlinx.serialization.json.*

/**
 * Endpoint persistence. MVP: private SharedPreferences.
 * TODO(slice-2): EncryptedSharedPreferences or Keystore-wrapped blob;
 * the token is the family silver — it unlocks a whole home.
 */
private const val PREFS = "hearths"
private const val KEY = "endpoints"

fun rememberEndpoint(ctx: Context): Endpoint? {
    val raw = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString(KEY, null)
        ?: return null
    return runCatching {
        val o = Json.parseToJsonElement(raw).jsonObject
        Endpoint(o["baseUrl"]!!.jsonPrimitive.content, o["token"]!!.jsonPrimitive.content, o["name"]!!.jsonPrimitive.content)
    }.getOrNull()
}

fun saveEndpoint(ctx: Context, ep: Endpoint) {
    ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit()
        .putString(KEY, buildJsonObject {
            put("baseUrl", ep.baseUrl); put("token", ep.token); put("name", ep.name)
        }.toString())
        .apply()
}
