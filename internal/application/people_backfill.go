package application

import "masamong/internal/memory/store"

func init() {
	featureDDL = append(featureDDL, store.PeopleBackfillDDL...)
}
