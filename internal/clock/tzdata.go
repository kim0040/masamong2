package clock

// Keep IANA timezone names available in portable builds, including Windows installs without Go.
import _ "time/tzdata"
