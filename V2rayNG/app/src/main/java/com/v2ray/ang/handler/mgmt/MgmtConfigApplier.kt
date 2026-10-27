package com.v2ray.ang.handler.mgmt

import android.content.Context
import com.google.gson.JsonArray
import com.google.gson.JsonObject
import com.v2ray.ang.AppConfig
import com.v2ray.ang.core.LauncherManager
import com.v2ray.ang.dto.entities.RulesetItem
import com.v2ray.ang.dto.entities.SubscriptionCache
import com.v2ray.ang.dto.entities.SubscriptionItem
import com.v2ray.ang.handler.AngConfigManager
import com.v2ray.ang.handler.MmkvManager
import com.v2ray.ang.handler.SettingsManager
import com.v2ray.ang.handler.SubscriptionUpdater
import com.v2ray.ang.helper.MessageHelper
import com.v2ray.ang.util.JsonUtil
import com.v2ray.ang.util.LogUtil
import java.security.MessageDigest

/**
 * Reconciles the device with the desired-configuration document from the management server
 * (schema in mgmt/server/API.md). Each section is applied independently; a failing section is
 * reported and does not stop the others.
 */
object MgmtConfigApplier {
    const val MANAGED_SUB_PREFIX = "mgmt-"
    const val MANAGED_SERVERS_GUID = "mgmt-servers"

    class Result(val version: String) {
        val changed = mutableListOf<String>()
        val errors = mutableListOf<String>()
        var needsRestart = false
        var restarted = false
        val subscriptionUpdates = JsonObject()

        fun toJson(): JsonObject = JsonObject().apply {
            addProperty("version", version)
            addProperty("ok", errors.isEmpty())
            add("changed", JsonArray().also { a -> changed.forEach { a.add(it) } })
            add("errors", JsonArray().also { a -> errors.forEach { a.add(it) } })
            addProperty("restarted", restarted)
            add("subscriptions", subscriptionUpdates)
            addProperty("ts", System.currentTimeMillis() / 1000.0)
        }
    }

    fun managedSubGuid(id: String): String =
        MANAGED_SUB_PREFIX + id.replace(Regex("[^A-Za-z0-9_.-]"), "_").take(64)

    private fun JsonObject.str(name: String): String? =
        if (has(name) && get(name).isJsonPrimitive) get(name).asString else null

    private fun JsonObject.bool(name: String): Boolean? =
        if (has(name) && get(name).isJsonPrimitive) get(name).asBoolean else null

    private fun JsonObject.obj(name: String): JsonObject? =
        if (has(name) && get(name).isJsonObject) getAsJsonObject(name) else null

    fun apply(context: Context, version: String, doc: JsonObject): Result {
        val r = Result(version)
        runSection(r, "settings") { applySettings(doc.obj("settings"), r) }
        runSection(r, "routing") { applyRouting(doc.obj("routing"), r) }
        runSection(r, "subscriptions") { applySubscriptions(context, doc.obj("subscriptions"), r) }
        runSection(r, "servers") { applyServers(doc.obj("servers"), r) }
        runSection(r, "select") { applySelect(doc.obj("select"), r) }

        LogUtil.refreshLogLevel()
        if (r.changed.isNotEmpty()) {
            MessageHelper.sendMsg2UI(context, AppConfig.MSG_MGMT_CONFIG_APPLIED, "")
        }
        val restartMode = doc.str("restart") ?: "auto"
        if (r.needsRestart && restartMode != "never") {
            // Only reaches a running daemon; a stopped VPN picks the new settings up when started.
            LauncherManager.restartService(context)
            r.restarted = MgmtSettings.isAgentAlive()
        }
        LogUtil.i(AppConfig.TAG, "Mgmt: applied config $version changed=${r.changed} errors=${r.errors.size}")
        return r
    }

    private inline fun runSection(r: Result, name: String, block: () -> Unit) {
        try {
            block()
        } catch (e: Exception) {
            LogUtil.e(AppConfig.TAG, "Mgmt: applying section $name failed", e)
            r.errors.add("$name: ${e.message ?: e.javaClass.simpleName}")
        }
    }

    private fun applySettings(settings: JsonObject?, r: Result) {
        settings ?: return
        for ((key, value) in settings.entrySet()) {
            try {
                if (MgmtSettingsRegistry.applyOne(key, value)) {
                    r.changed.add(key)
                    if (key !in MgmtSettingsRegistry.uiOnlyKeys) r.needsRestart = true
                }
            } catch (e: IllegalArgumentException) {
                r.errors.add("settings.$key: ${e.message}")
            }
        }
    }

    private fun applyRouting(routing: JsonObject?, r: Result) {
        routing ?: return
        if (!routing.has("rulesets")) return
        val wanted = JsonUtil.fromJson(routing.get("rulesets").toString(), Array<RulesetItem>::class.java)?.toMutableList()
            ?: throw IllegalArgumentException("rulesets must be an array of rules")
        val current = MmkvManager.decodeRoutingRulesets() ?: mutableListOf()
        if (JsonUtil.toJson(current) != JsonUtil.toJson(wanted)) {
            MmkvManager.encodeRoutingRulesets(wanted)
            r.changed.add("routing")
            r.needsRestart = true
        }
    }

    private fun managedSubs(): MutableSet<String> {
        val raw = MmkvManager.decodeSettingsString(MgmtSettings.KEY_MANAGED_SUBS)
        return JsonUtil.fromJsonSafe(raw.orEmpty(), Array<String>::class.java)?.toMutableSet() ?: mutableSetOf()
    }

    private fun applySubscriptions(context: Context, subs: JsonObject?, r: Result) {
        subs ?: return
        val items = if (subs.has("items") && subs.get("items").isJsonArray) subs.getAsJsonArray("items") else JsonArray()
        val updateOnChange = subs.bool("update_on_change") ?: true
        val wantedGuids = mutableSetOf<String>()
        val toUpdate = mutableListOf<String>()

        items.forEach { el ->
            val item = el.asJsonObject
            val id = item.str("id") ?: throw IllegalArgumentException("subscription without id")
            val guid = managedSubGuid(id)
            wantedGuids.add(guid)
            val existing = MmkvManager.decodeSubscription(guid)
            val next = (existing ?: SubscriptionItem()).copy(
                remarks = item.str("remarks") ?: existing?.remarks ?: id,
                url = item.str("url") ?: existing?.url.orEmpty(),
                enabled = item.bool("enabled") ?: true,
                autoUpdate = item.bool("auto_update") ?: true,
                updateInterval = item.get("update_interval_min")?.takeIf { it.isJsonPrimitive }?.asLong ?: existing?.updateInterval ?: 1440,
                filter = item.str("filter")?.ifBlank { null },
                userAgent = item.str("user_agent")?.ifBlank { null },
            )
            if (existing == null || JsonUtil.toJson(existing) != JsonUtil.toJson(next)) {
                MmkvManager.encodeSubscription(guid, next)
                r.changed.add("subscription:$id")
                if (existing == null || existing.url != next.url || MmkvManager.decodeServerList(guid).isEmpty()) {
                    toUpdate.add(guid)
                }
            }
        }

        val previous = managedSubs()
        (previous - wantedGuids).forEach { guid ->
            if (guid == MANAGED_SERVERS_GUID) return@forEach
            SettingsManager.removeSubscriptionWithDefault(guid)
            r.changed.add("subscription_removed:${guid.removePrefix(MANAGED_SUB_PREFIX)}")
            r.needsRestart = true
        }
        val keep = wantedGuids + previous.filter { it == MANAGED_SERVERS_GUID }
        MmkvManager.encodeSettings(MgmtSettings.KEY_MANAGED_SUBS, JsonUtil.toJson(keep.toList()))
        SubscriptionUpdater.sync(context, forceReschedule = toUpdate.isNotEmpty())

        if (updateOnChange) {
            toUpdate.forEach { guid ->
                val sub = MmkvManager.decodeSubscription(guid) ?: return@forEach
                val res = AngConfigManager.updateConfigViaSub(SubscriptionCache(guid, sub))
                r.subscriptionUpdates.addProperty(guid, "configs=${res.configCount} ok=${res.successCount} fail=${res.failureCount}")
                if (res.failureCount > 0) r.errors.add("subscription ${sub.remarks}: download failed")
            }
        }
    }

    private fun sha(s: String): String =
        MessageDigest.getInstance("SHA-256").digest(s.toByteArray()).joinToString("") { "%02x".format(it) }.take(16)

    private fun applyServers(servers: JsonObject?, r: Result) {
        servers ?: return
        val links = if (servers.has("links") && servers.get("links").isJsonArray)
            servers.getAsJsonArray("links").mapNotNull { if (it.isJsonPrimitive) it.asString.trim() else null }.filter { it.isNotEmpty() }
        else emptyList()
        val group = servers.str("group") ?: "Managed"
        val hash = sha(group + "\n" + links.joinToString("\n"))
        val haveServers = MmkvManager.decodeServerList(MANAGED_SERVERS_GUID).isNotEmpty()
        if (hash == MmkvManager.decodeSettingsString(MgmtSettings.KEY_SERVERS_HASH) && (haveServers || links.isEmpty())) return

        val sub = MmkvManager.decodeSubscription(MANAGED_SERVERS_GUID) ?: SubscriptionItem()
        MmkvManager.encodeSubscription(MANAGED_SERVERS_GUID, sub.copy(remarks = group, url = ""))
        val managed = managedSubs().apply { add(MANAGED_SERVERS_GUID) }
        MmkvManager.encodeSettings(MgmtSettings.KEY_MANAGED_SUBS, JsonUtil.toJson(managed.toList()))
        if (links.isEmpty()) {
            MmkvManager.removeServerViaSubid(MANAGED_SERVERS_GUID)
        } else {
            val (count, _) = AngConfigManager.importBatchConfig(links.joinToString("\n"), MANAGED_SERVERS_GUID, append = false)
            if (count <= 0) throw IllegalArgumentException("no valid share links in servers.links")
        }
        MmkvManager.encodeSettings(MgmtSettings.KEY_SERVERS_HASH, hash)
        r.changed.add("servers")
        r.needsRestart = true
    }

    /** Finds a server by remarks, optionally within a subscription given by managed id, GUID or remarks. */
    fun findServer(remarks: String, subscription: String?): String? {
        val subGuids: List<String> = if (subscription.isNullOrBlank()) {
            emptyList()
        } else {
            MmkvManager.decodeSubscriptions()
                .filter {
                    it.guid == subscription || it.guid == managedSubGuid(subscription) ||
                        it.subscription.remarks.equals(subscription, ignoreCase = true)
                }
                .map { it.guid }
        }
        val candidates = if (subGuids.isEmpty()) MmkvManager.decodeAllServerList() else subGuids.flatMap { MmkvManager.decodeServerList(it) }
        return candidates.firstOrNull { MmkvManager.decodeServerConfig(it)?.remarks == remarks }
            ?: candidates.firstOrNull { MmkvManager.decodeServerConfig(it)?.remarks?.trim().equals(remarks.trim(), ignoreCase = true) }
    }

    private fun applySelect(select: JsonObject?, r: Result) {
        select ?: return
        val remarks = select.str("remarks") ?: return
        val guid = findServer(remarks, select.str("subscription"))
            ?: throw IllegalArgumentException("no server named \"$remarks\"")
        if (MmkvManager.getSelectServer() != guid) {
            MmkvManager.setSelectServer(guid)
            r.changed.add("select")
            r.needsRestart = true
        }
    }
}
