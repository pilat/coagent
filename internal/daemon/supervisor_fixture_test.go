package daemon

import (
	"sync"
	"sync/atomic"

	"github.com/pilat/coagent/internal/admission"
)

var (
	fixtureRunnerID     atomic.Int64
	fixtureReservations sync.Map
)

type reservationKey struct {
	daemon   *svc
	kind     admission.Kind
	parentID int64
}

type reservedRunners struct {
	mu  sync.Mutex
	ids []int64
}

func (s *svc) reserveRunnerForTest(kind admission.Kind, parentID int64) bool {
	id := -fixtureRunnerID.Add(1)
	r := newRunner(func() { s.supervisor.Finish(id) }, "", 0, kind, parentID, false, nil)
	if !s.supervisor.Attach(id, r) {
		return false
	}
	value, _ := fixtureReservations.LoadOrStore(reservationKey{s, kind, parentID}, &reservedRunners{})
	reservations := value.(*reservedRunners)
	reservations.mu.Lock()
	reservations.ids = append(reservations.ids, id)
	reservations.mu.Unlock()
	return true
}

func (s *svc) releaseRunnerForTest(kind admission.Kind, parentID int64) {
	value, ok := fixtureReservations.Load(reservationKey{s, kind, parentID})
	if !ok {
		panic("release without runner reservation")
	}
	reservations := value.(*reservedRunners)
	reservations.mu.Lock()
	id := reservations.ids[len(reservations.ids)-1]
	reservations.ids = reservations.ids[:len(reservations.ids)-1]
	reservations.mu.Unlock()
	s.supervisor.Finish(id)
}

func (s *svc) canAdmitChildForTest(parentID int64) bool {
	if !s.reserveRunnerForTest(admission.Child, parentID) {
		return false
	}
	s.releaseRunnerForTest(admission.Child, parentID)
	return true
}
