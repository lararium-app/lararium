package app.lararium

import android.app.Activity
import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import app.lararium.data.Endpoint
import app.lararium.data.EndpointPolicy
import app.lararium.data.HearthClient
import app.lararium.data.PairingLink
import kotlinx.coroutines.launch

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val paired = rememberEndpoint(this)
        setContent {
            MaterialTheme {
                var ep by remember { mutableStateOf(paired) }
                // Handle lararium://pair deep links arriving cold or hot.
                LaunchedEffect(Unit) {
                    handlePairIntent(intent)?.let { ep = it }
                }
                val current = ep
                if (current == null) PairScreen { ep = it } else HearthScreen(current)
            }
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        // deep links while running: MainActivity is single-top via manifest launchMode
    }
}

fun Activity.handlePairIntent(intent: Intent?): Endpoint? {
    val data = intent?.data?.toString() ?: return null
    val (ep, _) = PairingLink.parse(data) ?: return null
    saveEndpoint(this, ep)
    return ep
}

@Composable
fun PairScreen(onPaired: (Endpoint) -> Unit) {
    var url by remember { mutableStateOf("") }
    var token by remember { mutableStateOf("") }
    var error by remember { mutableStateOf<String?>(null) }
    var testing by remember { mutableStateOf(false) }
    val scope = androidx.compose.runtime.rememberCoroutineScope()
    val context = androidx.compose.ui.platform.LocalContext.current

    Surface(Modifier.fillMaxSize()) {
        Column(Modifier.padding(24.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("Connect your hearth", style = MaterialTheme.typography.headlineSmall)
            Text(
                "Lararium only talks to your own server — LAN, tailnet, or your https endpoint.",
                style = MaterialTheme.typography.bodyMedium
            )
            OutlinedTextField(
                value = url, onValueChange = { url = it; error = null },
                label = { Text("hearthd URL (http://192.168.x.x:7717 or https://…)") },
                modifier = Modifier.fillMaxWidth(),
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri)
            )
            OutlinedTextField(
                value = token, onValueChange = { token = it; error = null },
                label = { Text("token (lar1_…)") },
                modifier = Modifier.fillMaxWidth(),
                visualTransformation = PasswordVisualTransformation()
            )
            error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
            Button(
                enabled = !testing,
                onClick = {
                    EndpointPolicy.rejectReason(url)?.let { error = it; return@Button }
                    if (!token.startsWith("lar1_") && !token.startsWith("lar0_")) {
                        error = "token looks wrong"; return@Button
                    }
                    testing = true
                    scope.launch {
                        HearthClient(Endpoint(url, token, "hearth")).health()
                            .onSuccess { h ->
                                if (h.ok) {
                                    val e = Endpoint(url, token, "hearth ${h.version}")
                                    saveEndpoint(context, e)
                                    onPaired(e)
                                } else error("hearthd said not-ok")
                            }
                            .onFailure { error = "can't reach hearthd: ${it.message}" }
                        testing = false
                    }
                }
            ) { Text(if (testing) "Checking…" else "Pair") }
        }
    }
}

@Composable
fun HearthScreen(ep: Endpoint) {
    var status by remember { mutableStateOf("checking…") }
    val scope = androidx.compose.runtime.rememberCoroutineScope()
    LaunchedEffect(ep) {
        HearthClient(ep).health()
            .onSuccess { status = "connected · hearthd ${it.version}" }
            .onFailure { status = "offline: ${it.message}" }
    }
    Surface(Modifier.fillMaxSize()) {
        Column(Modifier.padding(24.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text(ep.name, style = MaterialTheme.typography.headlineSmall)
            Text(ep.baseUrl, style = MaterialTheme.typography.bodySmall)
            Text(status)
            // Slice 1 continues: session list + chat SSE. Scaffold intentionally tiny here.
        }
    }
}
