package app.lararium.data

import java.io.BufferedReader
import java.io.IOException
import java.io.InputStream
import java.io.InputStreamReader
import java.io.OutputStream
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.Socket
import java.net.URL
import javax.net.ssl.SSLSocketFactory

/**
 * Cleartext is enforced HERE — the manifest keeps `usesCleartextTraffic=
 * "false"`, so the platform never opens a plain-HTTP socket for us. For the
 * LAN/tailnet http endpoints our policy allows, we open the socket
 * ourselves: string policy first, then a re-check on the RESOLVED addresses
 * (DNS-rebinding/TOCTOU), then a hand-rolled HTTP/1.1 GET (Android's
 * HttpURLConnection socket constructor is package-private; a raw client is
 * ~60 lines and buys exact SSE control for slice 2). https uses the normal
 * platform stack untouched.
 */
data class HttpResponse(val status: Int, val body: String)

object Net {
    fun get(urlString: String, headers: Map<String, String> = emptyMap(), timeoutMs: Int = 5_000): HttpResponse {
        EndpointPolicy.rejectReason(urlString)?.let { throw IOException(it) }
        val url = URL(urlString)
        val port = if (url.port == -1) if (url.protocol == "https") 443 else 80 else url.port
        val socket = when (url.protocol) {
            "https" -> (SSLSocketFactory.getDefault().createSocket() as javax.net.ssl.SSLSocket).also {
                // connect() by address does NOT imply hostname verification;
                // pin it explicitly so a valid cert for another host can't slide.
                it.sslParameters = it.sslParameters.apply {
                    endpointIdentificationAlgorithm = "HTTPS"
                }
                it.connect(InetSocketAddress(url.host, port), timeoutMs)
            }
            "http" -> {
                val addrs = try { InetAddress.getAllByName(url.host) } catch (e: Exception) {
                    throw IOException("dns failed for ${url.host}")
                }
                val private = addrs.isNotEmpty() && addrs.all {
                    it.isLoopbackAddress || it.isSiteLocalAddress || isCgnat(it)
                }
                if (!private) throw IOException("cleartext http is only allowed on LAN/tailnet hosts; use https")
                Socket().also { it.connect(InetSocketAddress(addrs[0], port), timeoutMs) }
            }
            else -> throw IOException("unsupported scheme ${url.protocol}")
        }
        socket.use { s ->
            s.soTimeout = timeoutMs + 15_000
            // Android's URL stores the encoded form directly in path/query.
            val path = (url.path.ifEmpty { "/" }) +
                (url.query?.let { "?$it" } ?: "")
            val req = buildString {
                append("GET ").append(path).append(" HTTP/1.1\r\n")
                append("Host: ").append(url.host); if (url.port != -1) append(":").append(url.port)
                append("\r\n")
                append("Accept-Encoding: identity\r\nConnection: close\r\n")
                headers.forEach { (k, v) -> append(k).append(": ").append(v).append("\r\n") }
                append("\r\n")
            }
            s.getOutputStream().write(req.toByteArray())
            s.getOutputStream().flush()
            return readResponse(s.getInputStream())
        }
    }

    private fun readResponse(input: InputStream): HttpResponse {
        val reader = BufferedReader(InputStreamReader(input, Charsets.UTF_8))
        val statusLine = reader.readLine() ?: throw IOException("empty response")
        val status = statusLine.substringAfter(' ').substringBefore(' ').toIntOrNull()
            ?: throw IOException("bad status line")
        val headers = mutableMapOf<String, String>()
        while (true) {
            val line = reader.readLine() ?: break
            if (line.isEmpty()) break
            val i = line.indexOf(':')
            if (i > 0) headers[line.substring(0, i).lowercase()] = line.substring(i + 1).trim()
        }
        val body = if (headers["transfer-encoding"]?.contains("chunked") == true) {
            buildString {
                while (true) {
                    val n = reader.readLine()?.trim()?.toIntOrNull(16) ?: break
                    if (n == 0) break
                    val buf = CharArray(n)
                    var read = 0
                    while (read < n) {
                        val r = reader.read(buf, read, n - read)
                        if (r < 0) break
                        read += r
                    }
                    append(buf, 0, read)
                    reader.readLine() // trailing CRLF
                }
            }
        } else reader.readText()
        return HttpResponse(status, body)
    }

    /** CGNAT / Tailscale range 100.64.0.0/10. */
    fun isCgnat(a: InetAddress): Boolean {
        val b = a.address
        return b.size == 4 && b[0] == 100.toByte() && b[1].toInt() and 0xFF in 64..127
    }
}
