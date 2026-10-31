package media

import (
	"github.com/pion/interceptor"
)

type pacerFactory struct{ pacer *BoundedPacer }

func NewPacerFactory(pacer *BoundedPacer) interceptor.Factory {
	return pacerFactory{pacer: pacer}
}

func (f pacerFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &pacerInterceptor{pacer: f.pacer}, nil
}

type pacerInterceptor struct {
	interceptor.NoOp
	pacer *BoundedPacer
}

func (i *pacerInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	i.pacer.AddStream(info.SSRC, writer)
	return i.pacer
}

func (i *pacerInterceptor) UnbindLocalStream(info *interceptor.StreamInfo) {
	i.pacer.RemoveStream(info.SSRC)
}
