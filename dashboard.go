package main

import "strings"

var dashboardHTML = strings.ReplaceAll(dashboardHead+dashboardStyles+dashboardBody+dashboardScripts+dashboardFoot, "__PLUGIN_ID__", pluginID)
