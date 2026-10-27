package com.v2ray.ang.ui.mgmt

import android.Manifest
import android.os.Build
import androidx.activity.result.contract.ActivityResultContracts
import androidx.activity.viewModels
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.consumeWindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Scaffold
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.v2ray.ang.R
import com.v2ray.ang.ui.base.BaseComponentActivity
import com.v2ray.ang.ui.compose.AppTopBar
import com.v2ray.ang.ui.compose.NavigationBarsSpacer
import com.v2ray.ang.ui.compose.PreferenceGroupHeader
import com.v2ray.ang.ui.compose.SettingsEditItem
import com.v2ray.ang.ui.compose.SettingsMenuItem
import com.v2ray.ang.ui.compose.SettingsSwitchItem
import com.v2ray.ang.util.Utils

/** Remote management (logs & configs server) settings and status. */
class MgmtActivity : BaseComponentActivity() {
    private val viewModel: MgmtViewModel by viewModels()

    private val permissionLauncher =
        registerForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { viewModel.syncNow() }

    private fun requestDiagnosticsPermissions() {
        val perms = mutableListOf(
            Manifest.permission.ACCESS_FINE_LOCATION,
            Manifest.permission.ACCESS_COARSE_LOCATION,
            Manifest.permission.READ_PHONE_STATE,
        )
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M) return
        permissionLauncher.launch(perms.toTypedArray())
    }

    @Composable
    override fun ScreenContent() {
        val state by viewModel.state.collectAsStateWithLifecycle()
        val statusText = when {
            !state.enabled -> stringResource(R.string.mgmt_status_off)
            state.status == "approved" -> stringResource(R.string.mgmt_status_approved)
            state.status == "pending" -> stringResource(R.string.mgmt_status_pending)
            state.status == "disabled" -> stringResource(R.string.mgmt_status_disabled)
            else -> stringResource(R.string.mgmt_status_unenrolled)
        }
        val lastSync = if (state.lastSyncMs > 0) Utils.formatTimestamp(state.lastSyncMs, "dd.MM HH:mm:ss") else stringResource(R.string.mgmt_never)
        val summary = buildString {
            append(stringResource(R.string.mgmt_status_value, statusText, lastSync))
            if (state.lastError.isNotBlank()) append("\n").append(stringResource(R.string.mgmt_last_error, state.lastError))
        }

        Scaffold(
            contentWindowInsets = WindowInsets(0),
            topBar = { AppTopBar(title = stringResource(R.string.title_mgmt), onBackClick = { finish() }) }
        ) { innerPadding ->
            val scroll = rememberScrollState()
            Column(
                modifier = Modifier
                    .fillMaxSize()
                    .padding(innerPadding)
                    .consumeWindowInsets(innerPadding)
                    .imePadding()
                    .verticalScroll(scroll)
            ) {
                SettingsSwitchItem(
                    title = stringResource(R.string.mgmt_enabled),
                    summary = stringResource(R.string.mgmt_enabled_summary),
                    checked = state.enabled,
                    onCheckedChange = viewModel::setEnabled
                )
                SettingsMenuItem(
                    title = stringResource(R.string.mgmt_status),
                    subtitle = summary,
                    onClick = viewModel::syncNow
                )
                SettingsMenuItem(
                    title = stringResource(R.string.mgmt_sync_now),
                    onClick = viewModel::syncNow
                )
                SettingsMenuItem(
                    title = stringResource(R.string.mgmt_grant_permissions),
                    subtitle = stringResource(R.string.mgmt_grant_permissions_summary),
                    onClick = ::requestDiagnosticsPermissions
                )

                PreferenceGroupHeader(title = stringResource(R.string.mgmt_section_device))
                SettingsEditItem(
                    title = stringResource(R.string.mgmt_device_name),
                    value = state.deviceName,
                    onValueChanged = viewModel::setDeviceName
                )
                SettingsMenuItem(
                    title = stringResource(R.string.mgmt_device_id),
                    subtitle = state.deviceId,
                    onClick = {}
                )

                PreferenceGroupHeader(title = stringResource(R.string.mgmt_section_server))
                SettingsEditItem(
                    title = stringResource(R.string.mgmt_url),
                    value = state.url,
                    onValueChanged = viewModel::setUrl
                )
                SettingsEditItem(
                    title = stringResource(R.string.mgmt_pin),
                    value = state.pin,
                    onValueChanged = viewModel::setPin
                )
                SettingsEditItem(
                    title = stringResource(R.string.mgmt_enroll_token),
                    value = state.enrollToken,
                    isPassword = true,
                    onValueChanged = viewModel::setEnrollToken
                )
                SettingsMenuItem(
                    title = stringResource(R.string.mgmt_reenroll),
                    subtitle = state.appliedVersion.ifBlank { null },
                    onClick = viewModel::reenroll
                )
                NavigationBarsSpacer()
            }
        }
    }
}
