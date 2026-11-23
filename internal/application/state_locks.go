package application

import "sync"

// stateLocks order the read-modify-write cycles of the service's own admin_state records. Each guards
// records of this service's store only, so it is a Service field (Service.stateLocks): the practice
// instance in the same process never waits for the real one.
type stateLocks struct {
	deliveryAlerted sync.Mutex // delivery:alerted (NoteDeliveryProblem, delivery.go)
	providerHealth  sync.Mutex // provider:<id> (noteProvider, ops_extra.go)
	statsInstall    sync.Mutex // the statistics install id (statsExportInstall, stats_export.go)
}
