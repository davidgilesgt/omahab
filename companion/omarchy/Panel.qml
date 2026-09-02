import QtQuick
import QtQuick.Controls
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui

Panel {
  id: root
  moduleName: "omahab.status"
  ipcTarget: "omahab.status"
  manageIpc: false

  property int cursorIndex: 0
  property bool cursorActive: false

  readonly property color foreground: bar ? bar.foreground : Color.foreground
  readonly property color urgent: bar ? bar.urgent : Color.urgent
  readonly property color dim: Qt.darker(foreground, 1.55)
  readonly property string fontFamily: bar ? bar.fontFamily : Style.font.family
  readonly property int attentionCount: client.unreadEvents > 0
    ? client.unreadEvents
    : client.waitingAgents + client.syncConflicts
  readonly property color stateColor: client.serverOnline ? foreground : urgent
  readonly property color barStateColor: client.serverOnline ? barForeground : urgent
  readonly property string statusMeta: {
    if (client.connecting) return "Connecting to the local client service"
    if (!client.clientdReachable) return "Disconnected · local client service unavailable"
    if (!client.hasStatus) return "Checking the private connection"
    if (client.serverOnline) return "Private connection ready"
    if (client.problemCount === 1) return "Server offline · 1 check needs attention"
    if (client.problemCount > 1) return "Server offline · " + client.problemCount + " checks need attention"
    return "Server offline"
  }

  readonly property var summaryRows: [
    { label: "Active runners", value: client.activeRunners, urgent: false },
    { label: "Waiting agents", value: client.waitingAgents, urgent: client.waitingAgents > 0 },
    { label: "Sync conflicts", value: client.syncConflicts, urgent: client.syncConflicts > 0 },
    { label: "Unread events", value: client.unreadEvents, urgent: client.unreadEvents > 0 },
    { label: "Tool variables", value: (client.environmentVariableCount + " · rev " + client.environmentRevision) + (client.environmentError !== "" ? " · error" : ""), urgent: client.environmentError !== "" }
  ]

  readonly property var baseActions: [
    { label: "Open AI", action: "open-ai", icon: "󰚩", requiresOnline: true },
    { label: "New Project", action: "project.new", icon: "󰙅", requiresOnline: true },
    { label: "Clone Project", action: "project.clone", icon: "󰜘", requiresOnline: true },
    { label: "Start or Resume Runner", action: "runner.start", icon: "󰆍", requiresOnline: true },
    { label: "Open Omahab", action: "open-omahab", icon: "󰖟", requiresOnline: true },
    { label: "Sync tool variables", action: "environment.sync", icon: "󰑓", requiresOnline: true },
    { label: "Diagnose Connection", action: "diagnose", icon: "󰒓", requiresOnline: false }
  ]

  readonly property var actions: {
    var list = baseActions.slice()
    if (client.hasXaiOAuthSession) {
      // Insert Connect xAI just before Diagnose to keep Diagnose last
      list.splice(list.length - 1, 0, { label: "Connect xAI subscription", action: "xai.oauth.connect", icon: "󰭹", requiresOnline: true })
    }
    return list
  }

  function actionEnabled(action) {
    if (client.actionBusy || !client.clientdReachable) return false
    return !action.requiresOnline || client.serverOnline
  }

  function ensureCursor() {
    cursorIndex = Math.max(0, Math.min(actions.length - 1, cursorIndex))
    if (actionEnabled(actions[cursorIndex])) return
    for (var i = 0; i < actions.length; i++) {
      var down = (cursorIndex + i) % actions.length
      if (actionEnabled(actions[down])) {
        cursorIndex = down
        return
      }
    }
  }

  function moveCursor(delta) {
    cursorActive = true
    if (actions.length === 0) return
    var start = cursorIndex
    do {
      cursorIndex = (cursorIndex + delta + actions.length) % actions.length
      if (actionEnabled(actions[cursorIndex])) return
    } while (cursorIndex !== start)
  }

  function selectAction(index) {
    cursorActive = true
    cursorIndex = index
  }

  function runAction(action) {
    if (!action || !actionEnabled(action)) return
    client.runAction(action.action, action.label)
    if (action.action !== "diagnose") root.close()
  }

  function activateCursor() {
    ensureCursor()
    runAction(actions[cursorIndex])
  }

  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  onOpenedChanged: if (opened) {
    cursorActive = false
    cursorIndex = 0
    client.actionStatus = ""
    client.refresh()
    Qt.callLater(function() { keyCatcher.forceActiveFocus() })
  }

  Clientd {
    id: client
    refreshIntervalSec: Math.max(5, Number(root.setting("refreshIntervalSec", 15)))
  }

  IpcHandler {
    target: root.ipcTarget
    function open(): void { root.open() }
    function close(): void { root.close() }
    function show(): void { root.open() }
    function hide(): void { root.close() }
    function toggle(): void { root.toggle() }
    function refresh(): string { client.refresh(); return "ok" }
    function diagnose(): string { client.runAction("diagnose", "Diagnose Connection"); return "ok" }
    function status(): string {
      if (!client.clientdReachable) return "clientd unavailable"
      return client.serverOnline ? "online" : "offline"
    }
  }

  BarIconButton {
    id: button
    anchors.fill: parent
    bar: root.bar
    Accessible.role: Accessible.Button
    Accessible.name: client.serverOnline ? "Omahab online" : "Omahab disconnected"
    Accessible.description: root.attentionCount > 0 ? root.attentionCount + " items need attention" : root.statusMeta

    iconComponent: Component {
      Item {
        Text {
          anchors.centerIn: parent
          text: "󰒍"
          color: root.barStateColor
          font.family: root.fontFamily
          font.pixelSize: Style.font.icon
          opacity: client.clientdReachable ? 1 : 0.5
        }

        Rectangle {
          visible: root.attentionCount > 0
          anchors.right: parent.right
          anchors.top: parent.top
          width: Style.space(11)
          height: width
          radius: width / 2
          color: root.urgent

          Text {
            anchors.centerIn: parent
            text: root.attentionCount > 9 ? "9+" : String(root.attentionCount)
            color: Color.background
            font.family: root.fontFamily
            font.pixelSize: Style.font.caption
            font.bold: true
          }
        }
      }
    }

    onPressed: function(buttonCode) {
      if (buttonCode === Qt.MiddleButton) client.refresh()
      else if (buttonCode === Qt.RightButton && client.clientdReachable)
        client.runAction("diagnose", "Diagnose Connection")
      else root.toggle()
    }
  }

  KeyboardPanel {
    id: panel
    anchorItem: button
    owner: root
    bar: root.bar
    open: root.opened
    focusTarget: keyCatcher
    contentWidth: panel.fittedContentWidth(Style.space(380))
    contentHeight: panel.fittedContentHeight(column.implicitHeight, Style.space(560))

    PanelKeyCatcher {
      id: keyCatcher
      anchors.fill: parent
      onMoveRequested: function(dx, dy) {
        if (!root.cursorActive) {
          root.cursorActive = true
          root.ensureCursor()
          return
        }
        if (dy !== 0) root.moveCursor(dy)
      }
      onActivateRequested: if (root.cursorActive) root.activateCursor()
      onCloseRequested: root.close()
      onTabRequested: function(direction) { root.switchPanel(direction) }
      onTextKey: function(text) {
        var key = String(text).toLowerCase()
        if (key === "r") client.refresh()
        else if (key === "d" && client.clientdReachable)
          client.runAction("diagnose", "Diagnose Connection")
      }

      Flickable {
        anchors.fill: parent
        contentWidth: width
        contentHeight: column.implicitHeight
        clip: true
        boundsBehavior: Flickable.StopAtBounds
        flickableDirection: Flickable.VerticalFlick
        interactive: contentHeight > height
        ScrollBar.vertical: ScrollBar { policy: ScrollBar.AsNeeded }

        Column {
          id: column
          width: parent.width
          spacing: Style.space(12)

          PanelHero {
            width: parent.width
            title: "Omahab"
            meta: root.statusMeta
            foreground: root.foreground
            fontFamily: root.fontFamily
            iconOpacity: client.serverOnline ? 1 : 0.5
            iconComponent: Component {
              Text {
                text: "󰒍"
                color: root.stateColor
                font.family: root.fontFamily
                font.pixelSize: Style.font.display
              }
            }
          }

          Text {
            visible: client.actionStatus !== ""
            width: parent.width
            text: client.actionStatus
            textFormat: Text.PlainText
            color: client.lastErrorCode === "" ? root.dim : root.urgent
            font.family: root.fontFamily
            font.pixelSize: Style.font.bodySmall
            wrapMode: Text.WordWrap
          }

          CursorSurface {
            visible: !client.connecting && !client.clientdReachable
            width: parent.width
            implicitHeight: disconnectedText.implicitHeight + Style.spacing.rowPaddingX
            foreground: root.foreground

            Text {
              id: disconnectedText
              anchors.left: parent.left
              anchors.right: parent.right
              anchors.verticalCenter: parent.verticalCenter
              anchors.margins: Style.space(12)
              text: "Start omahab-clientd to reconnect. This plugin never connects to the server directly."
              textFormat: Text.PlainText
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
              wrapMode: Text.WordWrap
            }
          }

          Column {
            visible: client.hasStatus
            width: parent.width
            spacing: Style.space(6)

            PanelSectionHeader {
              text: "STATUS"
              foreground: root.foreground
              fontFamily: root.fontFamily
            }

            Repeater {
              model: root.summaryRows

              Item {
                required property var modelData
                width: parent.width
                implicitHeight: Math.max(
                  Style.spacing.popupRowHeight,
                  metricLabel.implicitHeight,
                  metricValue.implicitHeight)

                Text {
                  id: metricLabel
                  anchors.left: parent.left
                  anchors.verticalCenter: parent.verticalCenter
                  text: modelData.label
                  textFormat: Text.PlainText
                  color: root.dim
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.body
                }

                Text {
                  id: metricValue
                  anchors.right: parent.right
                  anchors.verticalCenter: parent.verticalCenter
                  text: String(modelData.value)
                  textFormat: Text.PlainText
                  color: modelData.urgent ? root.urgent : root.foreground
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.body
                  font.bold: modelData.urgent
                }
              }
            }
          }

          PanelSeparator {
            foreground: root.foreground
          }

          Column {
            width: parent.width
            spacing: Style.space(6)

            PanelSectionHeader {
              text: "ACTIONS"
              foreground: root.foreground
              fontFamily: root.fontFamily
            }

            Repeater {
              model: root.actions

              Button {
                required property var modelData
                required property int index
                width: parent.width
                text: modelData.label
                iconText: modelData.icon
                leftAlign: true
                foreground: root.foreground
                accent: Color.accent
                fontFamily: root.fontFamily
                enabled: root.actionEnabled(modelData)
                selected: index === 0 && enabled
                hasCursor: root.cursorActive && root.cursorIndex === index
                bordered: index === 0
                Accessible.role: Accessible.Button
                Accessible.name: modelData.label
                Accessible.description: enabled ? "Runs through the local Omahab client service" : "Unavailable while disconnected"
                onHovered: function(hovered) { if (hovered && enabled) root.selectAction(index) }
                onClicked: root.runAction(modelData)
              }
            }
          }

          Text {
            visible: client.clientdReachable && !client.serverOnline
            width: parent.width
            text: "Only Diagnose is available until the private connection is ready."
            textFormat: Text.PlainText
            color: root.dim
            font.family: root.fontFamily
            font.pixelSize: Style.font.bodySmall
            wrapMode: Text.WordWrap
          }
        }
      }
    }
  }
}
