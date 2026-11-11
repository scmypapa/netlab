package engine

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

func guestFileCommand(os, path, content string) map[string]any {
	command := map[string]any{"capture-output": true}
	if strings.EqualFold(os, "windows") {
		payload, _ := json.Marshal(map[string]string{"path": path, "content": content})
		command["path"] = "powershell.exe"
		command["arg"] = []string{"-NoProfile", "-NonInteractive", "-Command", `$ErrorActionPreference='Stop'; [Console]::InputEncoding=[Text.UTF8Encoding]::new($false); $file=[Console]::In.ReadToEnd() | ConvertFrom-Json; [IO.Directory]::CreateDirectory([IO.Path]::GetDirectoryName($file.path)) | Out-Null; [IO.File]::WriteAllText($file.path,$file.content,[Text.UTF8Encoding]::new($false))`}
		command["input-data"] = base64.StdEncoding.EncodeToString(payload)
	} else {
		command["path"] = "/bin/sh"
		command["arg"] = []string{"-c", `umask 022; mkdir -p -- "$(dirname -- "$1")" && cat > "$1"`, "netlab-file", path}
		command["input-data"] = base64.StdEncoding.EncodeToString([]byte(content))
	}
	return command
}
