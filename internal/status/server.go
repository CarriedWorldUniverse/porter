package status

import (
	"context"

	cwbv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Server serves cwb.v1.BackupStatusService from a Holder.
type Server struct {
	cwbv1.UnimplementedBackupStatusServiceServer
	h *Holder
}

// NewServer wires the service to the holder.
func NewServer(h *Holder) *Server { return &Server{h: h} }

// GetBackupStatus reports the last pass. No authz beyond mesh mTLS: the data
// is operational metadata, and the mesh boundary is the trust boundary.
func (s *Server) GetBackupStatus(_ context.Context, _ *cwbv1.GetBackupStatusRequest) (*cwbv1.GetBackupStatusResponse, error) {
	snap := s.h.Get()
	st := &cwbv1.BackupStatus{LastError: snap.LastError}
	if snap.LastSuccess != nil {
		st.LastSuccess = timestamppb.New(*snap.LastSuccess)
	}
	if snap.LastAttempt != nil {
		st.LastAttempt = timestamppb.New(*snap.LastAttempt)
	}
	if !snap.NextDue.IsZero() {
		st.NextDue = timestamppb.New(snap.NextDue)
	}
	for _, src := range snap.Sources {
		st.Sources = append(st.Sources, &cwbv1.BackupSource{Name: src.Name, SizeBytes: src.SizeBytes})
	}
	return &cwbv1.GetBackupStatusResponse{Status: st}, nil
}
