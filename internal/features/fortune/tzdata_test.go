package fortune

// The zone-dependent tests (Seoul's historical offsets, New York, Madrid) need the IANA database. Link the
// embedded copy so they run, and fail rather than skip, on a machine with no zoneinfo (Windows, a
// minimal container).
import _ "time/tzdata"
