package tui

// Key bindings for the overview. Kept minimal and documented in the footer.
// Arrow keys and vim k/j both move the selection.
const (
	keyUp      = "up"
	keyDown    = "down"
	keyVimUp   = "k"
	keyVimDown = "j"
	keyProc    = "p" // rclone process count (SSM)
	keyDetail  = "t" // rclone transfer detail (SSM)
	keyRefresh = "R" // rolling instance refresh (guarded)
	keyReload  = "r" // reload overview now
	keyQuit    = "q"
)
