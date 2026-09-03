import QtQuick
import Quickshell
import Quickshell.Io

Item {
  id: root

  property int refreshIntervalSec: 15

  readonly property string socketPath: {
    var configured = String(Quickshell.env("OMAHAB_SOCKET") || "")
    if (configured !== "") return configured
    var runtimeDir = String(Quickshell.env("XDG_RUNTIME_DIR") || "")
    if (runtimeDir !== "") return runtimeDir + "/omahab-clientd.sock"
    var home = String(Quickshell.env("HOME") || "")
    return home !== "" ? home + "/.cache/omahab/clientd.sock" : "/tmp/omahab-clientd.sock"
  }

  property bool connecting: true
  property bool clientdReachable: false
  property bool serverOnline: false
  property bool hasStatus: false
  property bool actionBusy: false
  property string actionStatus: ""
  property string lastErrorCode: ""
  property int activeRunners: 0
  property int waitingAgents: 0
  property int syncConflicts: 0
  property int unreadEvents: 0
  property string checkedAt: ""
  property int problemCount: 0
  property int environmentRevision: 0
  property int environmentVariableCount: 0
  property string environmentSyncedAt: ""
  property string environmentError: ""
  property bool hasXaiOAuthSession: false
  property var workspaces: []
  property var projects: []

  property var requestQueue: []

  signal actionFinished(string action, bool succeeded)

  function boundedCount(value) {
    var number = Number(value)
    return isFinite(number) && number > 0 ? Math.floor(number) : 0
  }

  function setUnavailable() {
    connecting = false
    clientdReachable = false
    serverOnline = false
    hasStatus = false
    activeRunners = 0
    waitingAgents = 0
    syncConflicts = 0
    unreadEvents = 0
    problemCount = 0
    environmentRevision = 0
    environmentVariableCount = 0
    environmentSyncedAt = ""
    environmentError = ""
    hasXaiOAuthSession = false
    workspaces = []
    projects = []
  }

  function hasQueuedKind(kind) {
    if (currentRequest && currentRequest.kind === kind) return true
    for (var i = 0; i < requestQueue.length; i++)
      if (requestQueue[i].kind === kind) return true
    return false
  }

  function enqueue(method, params, kind, label) {
    if (kind === "status" && hasQueuedKind("status")) return
    var next = requestQueue.slice()
    next.push({
      id: "omarchy-" + nextRequestId++,
      method: method,
      params: params || ({}),
      kind: kind,
      label: label
    })
    requestQueue = next
    startNext()
  }

  function startNext() {
    if (currentRequest || requestQueue.length === 0 || clientSocket.connected) return
    var next = requestQueue.slice()
    currentRequest = next.shift()
    requestQueue = next
    connecting = !clientdReachable
    clientSocket.connected = true
  }

  function refresh() {
    enqueue("status", {}, "status", "")
    // also refresh workspaces and projects for the picker and live list
    Qt.callLater(function() {
      enqueue("workspace.list", {}, "workspace_list", "")
      enqueue("project.list", {}, "project_list", "")
    })
  }

  function refreshWorkspaces() {
    enqueue("workspace.list", {}, "workspace_list", "")
  }

  function refreshProjects() {
    enqueue("project.list", {}, "project_list", "")
  }

  function workspaceCreate(projectSlug, title) {
    if (actionBusy) return
    actionBusy = true
    actionStatus = ""
    enqueue("workspace.create", {project_slug: projectSlug, title: title}, "action", "Create workspace")
  }

  function workspaceAttach(id) {
    enqueue("workspace.attach", {id: id}, "action", "Attach workspace")
  }

  function workspaceStop(id) {
    enqueue("workspace.stop", {id: id}, "action", "Stop workspace")
  }

  function runAction(method, label) {
    if (actionBusy) return
    actionBusy = true
    actionStatus = ""
    enqueue(method, {}, "action", label)
  }

  function sendCurrent() {
    if (!currentRequest) return
    clientSocket.write(JSON.stringify({
      id: currentRequest.id,
      method: currentRequest.method,
      params: currentRequest.params
    }) + "\n")
    clientSocket.flush()
    responseTimeout.interval = currentRequest.kind === "action" ? 12000 : 5000
    responseTimeout.restart()
  }

  function failCurrent(code) {
    responseTimeout.stop()
    var failed = currentRequest
    currentRequest = null
    lastErrorCode = code
    if (failed && failed.kind === "action") {
      actionBusy = false
      actionStatus = failed.label + " failed. Run Diagnose for details."
      actionFinished(failed.label, false)
    } else {
      setUnavailable()
    }
    if (clientSocket.connected) {
      expectedDisconnect = true
      clientSocket.connected = false
    }
    reconnectTimer.restart()
  }

  function handleResponse(line) {
    var text = String(line || "").trim()
    if (text === "" || !currentRequest) return

    var response
    try {
      response = JSON.parse(text)
    } catch (error) {
      failCurrent("invalid_clientd_response")
      return
    }

    responseTimeout.stop()
    var completed = currentRequest
    if (String(response.id || "") !== completed.id) {
      failCurrent("mismatched_clientd_response")
      return
    }
    currentRequest = null
    expectedDisconnect = true

    if (completed.kind === "status") {
      connecting = false
      clientdReachable = true
      if (response.error) {
        hasStatus = false
        serverOnline = false
        lastErrorCode = String(response.error.code || "status_failed")
      } else {
        applyStatus(response.result || ({}))
      }
    } else if (completed.kind === "workspace_list") {
      if (response.error) {
        workspaces = []
      } else {
        var r = response.result
        if (r && r.items instanceof Array) workspaces = r.items
        else if (r instanceof Array) workspaces = r
        else if (r && r.workspaces instanceof Array) workspaces = r.workspaces
        else workspaces = []
      }
    } else if (completed.kind === "project_list") {
      if (response.error) {
        projects = []
      } else {
        var r2 = response.result
        if (r2 && r2.items instanceof Array) projects = r2.items
        else if (r2 instanceof Array) projects = r2
        else if (r2 && r2.projects instanceof Array) projects = r2.projects
        else projects = []
      }
    } else {
      actionBusy = false
      if (response.error) {
        lastErrorCode = String(response.error.code || "action_failed")
        actionStatus = completed.label + " failed. Run Diagnose for details."
        actionFinished(completed.label, false)
      } else {
        lastErrorCode = ""
        applyActionResult(completed.method, completed.label, response.result || ({}))
        actionFinished(completed.label, true)
        refreshDelay.restart()
      }
    }

    if (clientSocket.connected) clientSocket.connected = false
    Qt.callLater(startNext)
  }

  function countUnreadEvents(events, eventType) {
    if (!(events instanceof Array)) return 0
    var count = 0
    for (var i = 0; i < events.length; i++) {
      var event = events[i] || ({})
      if ((event.read_at === undefined || event.read_at === null || event.read_at === "")
          && (eventType === "" || String(event.type || "") === eventType)) count++
    }
    return count
  }

  function applyStatus(status) {
    hasStatus = true
    lastErrorCode = ""
    serverOnline = status.online === true
    activeRunners = boundedCount(status.active_runners !== undefined ? status.active_runners : status.activeRunners)
    waitingAgents = boundedCount(status.waiting_agents !== undefined
      ? status.waiting_agents
      : (status.waitingAgents !== undefined ? status.waitingAgents : countUnreadEvents(status.events, "agent.awaiting_approval")))
    syncConflicts = boundedCount(status.sync_conflicts !== undefined
      ? status.sync_conflicts
      : (status.syncConflicts !== undefined ? status.syncConflicts : countUnreadEvents(status.events, "syncthing.conflict")))
    unreadEvents = boundedCount(status.unread_events !== undefined
      ? status.unread_events
      : (status.unreadEvents !== undefined ? status.unreadEvents
        : (status.unread_count !== undefined ? status.unread_count : countUnreadEvents(status.events, ""))))
    checkedAt = String(status.checked_at || status.checkedAt || status.last_sync_at || status.environment_synced_at || "")
    problemCount = String(status.error || "") === "" ? 0 : 1
    environmentRevision = boundedCount(status.environment_revision !== undefined ? status.environment_revision : status.environmentRevision)
    environmentVariableCount = boundedCount(status.environment_variable_count !== undefined ? status.environment_variable_count : status.environmentVariableCount)
    environmentSyncedAt = String(status.environment_synced_at !== undefined ? status.environment_synced_at : (status.environmentSyncedAt || status.checked_at || ""))
    environmentError = String(status.environment_error !== undefined ? status.environment_error : (status.environmentError || ""))
    // xAI loopback OAuth session assigned to this device — server reports via status field when active
    var xaiFlag = status.xai_oauth_active !== undefined ? status.xai_oauth_active
      : status.has_xai_oauth_session !== undefined ? status.has_xai_oauth_session
      : status.xaiOAuthActive !== undefined ? status.xaiOAuthActive
      : status.companion_has_xai_oauth !== undefined ? status.companion_has_xai_oauth
      : false
    // Also consider oauth_sessions array or pending provider hint
    if (!xaiFlag && status.oauth_sessions instanceof Array) {
      for (var i = 0; i < status.oauth_sessions.length; i++) {
        var s = status.oauth_sessions[i] || {}
        if (String(s.provider || "") === "xai" && String(s.status || "") === "pending" && s.assigned_to_device === true) {
          xaiFlag = true
          break
        }
        if (String(s.provider || "") === "xai" && String(s.flow || "") === "loopback" && String(s.status || "") === "pending") {
          xaiFlag = true
          break
        }
      }
    }
    if (!xaiFlag && String(status.oauth_pending_provider || "") === "xai") xaiFlag = true
    hasXaiOAuthSession = xaiFlag === true
  }

  function applyActionResult(action, label, data) {
    if (action === "diagnose") {
      var checks = data.checks instanceof Array ? data.checks : []
      var failures = 0
      for (var i = 0; i < checks.length; i++) if (checks[i].ok !== true) failures++
      actionStatus = failures === 0
        ? "All connection checks passed"
        : failures + (failures === 1 ? " connection check needs attention" : " connection checks need attention")
      return
    }
    if (action === "environment.sync" || action === "environment_sync" || action === "env.sync" || action === "environment_sync" || label === "Sync tool variables") {
      actionStatus = "Applied to new apps; restart existing apps"
      return
    }
    actionStatus = label + " opened"
  }

  Socket {
    id: clientSocket
    path: root.socketPath
    connected: false
    parser: SplitParser {
      splitMarker: "\n"
      onRead: function(line) { root.handleResponse(line) }
    }

    onConnectionStateChanged: {
      if (connected) {
        reconnectTimer.stop()
        root.sendCurrent()
      } else if (root.expectedDisconnect) {
        root.expectedDisconnect = false
        Qt.callLater(root.startNext)
      } else if (root.currentRequest) {
        root.failCurrent("clientd_disconnected")
      }
    }

    onError: function(error) {
      if (root.expectedDisconnect) return
      if (root.currentRequest) root.failCurrent("clientd_unavailable")
      else {
        root.setUnavailable()
        reconnectTimer.restart()
      }
    }
  }

  Timer {
    id: pollTimer
    interval: Math.max(5, root.refreshIntervalSec) * 1000
    repeat: true
    running: true
    onTriggered: root.refresh()
  }

  Timer {
    id: reconnectTimer
    interval: 5000
    repeat: false
    onTriggered: root.refresh()
  }

  Timer {
    id: responseTimeout
    interval: 5000
    repeat: false
    onTriggered: root.failCurrent("clientd_timeout")
  }

  Timer {
    id: refreshDelay
    interval: 500
    repeat: false
    onTriggered: root.refresh()
  }

  Component.onCompleted: refresh()
}
