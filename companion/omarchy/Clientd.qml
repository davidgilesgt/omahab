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

  property var requestQueue: []
  property var currentRequest: null
  property bool expectedDisconnect: false
  property int nextRequestId: 1


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
    checkedAt = String(status.checked_at || status.checkedAt || status.last_sync_at || "")
    problemCount = String(status.error || "") === "" ? 0 : 1
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
