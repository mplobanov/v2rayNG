package com.v2ray.ang.ui.mgmt

import android.app.Application
import androidx.lifecycle.viewModelScope
import com.v2ray.ang.R
import com.v2ray.ang.handler.MmkvManager
import com.v2ray.ang.handler.mgmt.MgmtSettings
import com.v2ray.ang.handler.mgmt.MgmtSyncWorker
import com.v2ray.ang.ui.base.BaseViewModel
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

data class MgmtUiState(
    val enabled: Boolean = false,
    val url: String = "",
    val pin: String = "",
    val enrollToken: String = "",
    val deviceName: String = "",
    val deviceId: String = "",
    val status: String = "",
    val lastSyncMs: Long = 0L,
    val lastError: String = "",
    val appliedVersion: String = "",
)

class MgmtViewModel(application: Application) : BaseViewModel(application) {
    private val _state = MutableStateFlow(MgmtUiState())
    val state: StateFlow<MgmtUiState> = _state.asStateFlow()

    init {
        reload()
        // The sync runs in another process; refresh the status line while the screen is open.
        viewModelScope.launch {
            while (true) {
                delay(3_000)
                reload()
            }
        }
    }

    fun reload() {
        viewModelScope.launch(Dispatchers.IO) {
            _state.value = MgmtUiState(
                enabled = MgmtSettings.isEnabled(),
                url = MgmtSettings.url(),
                pin = MgmtSettings.pin(),
                enrollToken = MgmtSettings.enrollToken(),
                deviceName = MgmtSettings.deviceName(),
                deviceId = MgmtSettings.deviceId(),
                status = if (MgmtSettings.deviceToken() == null) "" else MgmtSettings.status(),
                lastSyncMs = MgmtSettings.lastSync(),
                lastError = MgmtSettings.lastError(),
                appliedVersion = MgmtSettings.appliedVersion(),
            )
        }
    }

    private fun update(key: String, value: String, resetEnrollment: Boolean = false) {
        viewModelScope.launch(Dispatchers.IO) {
            MmkvManager.encodeSettings(key, value.trim())
            if (resetEnrollment) MgmtSettings.forgetEnrollment()
            reload()
            MgmtSyncWorker.schedule(app)
            MgmtSyncWorker.enqueueNow(app)
        }
    }

    fun setEnabled(enabled: Boolean) {
        viewModelScope.launch(Dispatchers.IO) {
            MmkvManager.encodeSettings(MgmtSettings.KEY_ENABLED, enabled)
            reload()
            MgmtSyncWorker.schedule(app)
            if (enabled) MgmtSyncWorker.enqueueNow(app)
        }
    }

    fun setUrl(value: String) = update(MgmtSettings.KEY_URL, value, resetEnrollment = true)
    fun setPin(value: String) = update(MgmtSettings.KEY_PIN, value)
    fun setEnrollToken(value: String) = update(MgmtSettings.KEY_ENROLL_TOKEN, value)
    fun setDeviceName(value: String) = update(MgmtSettings.KEY_DEVICE_NAME, value)

    fun syncNow() {
        viewModelScope.launch(Dispatchers.IO) {
            MgmtSyncWorker.enqueueNow(app)
        }
        toast(R.string.mgmt_sync_started)
    }

    fun reenroll() {
        viewModelScope.launch(Dispatchers.IO) {
            MgmtSettings.forgetEnrollment()
            MgmtSyncWorker.enqueueNow(app)
            reload()
        }
        toast(R.string.mgmt_sync_started)
    }
}
