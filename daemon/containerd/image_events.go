package containerd

import (
	"context"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/docker/docker/daemon/server/backend"
	"github.com/moby/moby/api/types/events"
)

// LogImageEvent generates an event related to an image with only the default attributes.
func (i *ImageService) LogImageEvent(ctx context.Context, imageID, refName string, action events.Action) {
	ctx = context.WithoutCancel(ctx)
	attributes := map[string]string{}

	img, err := i.GetImage(ctx, imageID, backend.GetImageOpts{})
	if err == nil && img.Config != nil {
		// image has not been removed yet.
		// it could be missing if the event is `delete`.
		copyAttributes(attributes, img.Config.Labels)
	}
	if refName != "" {
		attributes["name"] = refName
	}
	i.eventsService.Log(action, events.ImageEventType, events.Actor{
		ID:         imageID,
		Attributes: attributes,
	})
}

// logImageEvent generates an event related to an image with only name attribute.
func (i *ImageService) logImageEvent(img c8dimages.Image, refName string, action events.Action) {
	attributes := map[string]string{}
	if refName != "" {
		attributes["name"] = refName
	}
	i.eventsService.Log(action, events.ImageEventType, events.Actor{
		ID:         img.Target.Digest.String(),
		Attributes: attributes,
	})
}

// copyAttributes guarantees that labels are not mutated by event triggers.
func copyAttributes(attributes, labels map[string]string) {
	if labels == nil {
		return
	}
	for k, v := range labels {
		attributes[k] = v
	}
}
