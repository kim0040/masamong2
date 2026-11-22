package application_test

// The fortune tests need the IANA database. Link the embedded copy so they run, and fail rather than
// skip, on a machine with no zoneinfo (Windows, a minimal container).
import _ "time/tzdata"
