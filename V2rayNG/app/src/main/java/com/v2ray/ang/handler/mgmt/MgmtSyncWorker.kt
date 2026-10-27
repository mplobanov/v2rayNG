package com.v2ray.ang.handler.mgmt

import android.content.Context
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkerParameters
import androidx.work.multiprocess.RemoteWorkManager
import androidx.work.workDataOf
import com.v2ray.ang.AppConfig
import com.v2ray.ang.handler.MmkvManager
import com.v2ray.ang.util.LogUtil
import java.util.concurrent.TimeUnit

/**
 * Syncs with the management server while the VPN (and so the daemon agent) is not running:
 * reports state and events, applies configuration, runs commands that need no core, and uploads
 * the log lines written since the last upload. Skips itself while the daemon agent is alive.
 */
class MgmtSyncWorker(context: Context, params: WorkerParameters) : CoroutineWorker(context, params) {

    override suspend fun doWork(): Result {
        if (!MgmtSettings.isEnabled()) return Result.success()
        val forced = inputData.getBoolean(KEY_FORCE, false)
        val agentAlive = MgmtSettings.isAgentAlive()
        if (agentAlive && !forced) return Result.success()

        val ctx = applicationContext
        val core = MgmtTelemetry.CoreFacts(running = agentAlive, inDaemon = false)
        val events = MgmtEvents.drain()
        var outcome = MgmtClient.sync(ctx, core, events, full = forced)
        if (!outcome.ok) {
            MgmtEvents.requeue(events)
            LogUtil.w(AppConfig.TAG, "Mgmt: idle sync failed: ${outcome.error}")
            return Result.success()
        }
        var fullRequested = false
        val env = MgmtCommands.Env(ctx, coreRunning = false, coreDelay = null, requestSync = { fullRequested = fullRequested || it })
        val results = outcome.commands.mapNotNull { cmd -> MgmtCommands.executeOnce(env, cmd) { id -> MgmtClient.ack(listOf(id)) } }
        if (results.isNotEmpty() && !MgmtClient.postResults(results)) {
            outcome = MgmtClient.sync(ctx, core, results = results)
        }
        if (fullRequested) MgmtClient.sync(ctx, core, full = true)

        if (!agentAlive) {
            val spec = MgmtSettings.agentString("log_spec", "*:W GoLog:V {app}:V")
            val lines = MgmtLogShipper.dump(
                spec, MgmtLogShipper.lastUploadedTs(), maxLines = 20_000, dnsCacheLines = MgmtSettings.agentBool("dns_log_cache", false)
            )
            lines.chunked(5000).forEach { if (!MgmtLogShipper.upload(it, 0, "idle")) return@forEach }
        }
        schedule(ctx)
        val pendingChecks = inputData.getInt(KEY_PENDING_CHECKS, 0)
        if (MgmtSettings.status() == "pending" && pendingChecks > 0) {
            enqueuePendingCheck(ctx, pendingChecks - 1)
        }
        return Result.success()
    }

    companion object {
        private const val PERIODIC = "mgmt_sync"
        private const val NOW = "mgmt_sync_now"
        private const val PENDING = "mgmt_sync_pending"
        private const val KEY_FORCE = "force"
        private const val KEY_PENDING_CHECKS = "pending_checks"

        /** A device waiting for approval checks back once a minute for this many minutes after enqueueNow. */
        private const val PENDING_CHECKS = 30
        private const val KEY_SCHEDULED_MIN = "mgmt_scheduled_interval"

        private val constraints = Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build()

        /** Keeps the periodic idle sync scheduled at the interval the server asked for. */
        fun schedule(context: Context) {
            if (!MgmtSettings.isEnabled()) {
                RemoteWorkManager.getInstance(context).cancelUniqueWork(PERIODIC)
                return
            }
            val minutes = MgmtSettings.agentInt("idle_sync_min", 15).coerceAtLeast(15).toLong()
            val previous = MmkvManager.decodeSettingsLong(KEY_SCHEDULED_MIN, 0L)
            val policy = if (previous == minutes) ExistingPeriodicWorkPolicy.KEEP else ExistingPeriodicWorkPolicy.UPDATE
            val request = PeriodicWorkRequestBuilder<MgmtSyncWorker>(minutes, TimeUnit.MINUTES)
                .setConstraints(constraints)
                .build()
            RemoteWorkManager.getInstance(context).enqueueUniquePeriodicWork(PERIODIC, policy, request)
            MmkvManager.encodeSettings(KEY_SCHEDULED_MIN, minutes)
        }

        /** Runs one sync as soon as there is network (app opened, settings changed, VPN start failed). */
        fun enqueueNow(context: Context, force: Boolean = true) {
            if (!MgmtSettings.isEnabled()) return
            val request = OneTimeWorkRequestBuilder<MgmtSyncWorker>()
                .setConstraints(constraints)
                .setInputData(workDataOf(KEY_FORCE to force, KEY_PENDING_CHECKS to PENDING_CHECKS))
                .build()
            RemoteWorkManager.getInstance(context).enqueueUniqueWork(NOW, ExistingWorkPolicy.REPLACE, request)
        }

        /**
         * Lets an approval reach a device whose VPN is off without the family member reopening the
         * app: the periodic idle sync alone would leave it waiting up to [schedule]'s interval.
         */
        private fun enqueuePendingCheck(context: Context, checksLeft: Int) {
            val request = OneTimeWorkRequestBuilder<MgmtSyncWorker>()
                .setConstraints(constraints)
                .setInitialDelay(1, TimeUnit.MINUTES)
                .setInputData(workDataOf(KEY_FORCE to false, KEY_PENDING_CHECKS to checksLeft))
                .build()
            RemoteWorkManager.getInstance(context).enqueueUniqueWork(PENDING, ExistingWorkPolicy.REPLACE, request)
        }
    }
}
