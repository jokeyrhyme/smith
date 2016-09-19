package app

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/ash2k/smith"
	"github.com/ash2k/smith/pkg/client"
	"github.com/ash2k/smith/pkg/processor"
)

type App struct {
	Watcher   *Watcher
	Client    *client.ResourceClient
	Processor *processor.TemplateProcessor
	Events    <-chan interface{}
}

func (a *App) Run(ctx context.Context) error {
	// 1. Ensure ThirdPartyResource TEMPLATE exists
	err := retryUntilSuccessOrDone(ctx, func() error {
		return ensureResourceExists(ctx, a.Client)
	}, func(e error) bool {
		// TODO be smarter about what is retried
		log.Printf("Failed to create resource %s: %v", smith.TemplateResourceName, e)
		return false
	})
	if err != nil {
		return err
	}

	// 2. TODO watch supported built-in resource types for events.

	// 3. List Third Party Resources to figure out list of supported ones
	var tprList smith.ThirdPartyResourceList
	err = retryUntilSuccessOrDone(ctx, func() error {
		tprList = smith.ThirdPartyResourceList{}
		return a.Client.List(ctx, smith.ThirdPartyResourceGroupVersion, smith.AllNamespaces, smith.AllResources, nil, nil, &tprList)
	}, func(e error) bool {
		// TODO be smarter about what is retried
		log.Printf("Failed to list Third Party Resources %s: %v", smith.TemplateResourceName, e)
		return false
	})
	if err != nil {
		return err
	}
	for _, tpr := range tprList.Items {
		a.watchTpr(&tpr)
	}

	// 4. Watch for addition/removal of TPRs to start/stop watches
	a.Watcher.Watch(smith.ThirdPartyResourceGroupVersion, smith.AllNamespaces, smith.AllResources, tprList.ResourceVersion, newTprEvent)

	// 5. List existing templates
	var templateList smith.TemplateList
	err = retryUntilSuccessOrDone(ctx, func() error {
		templateList = smith.TemplateList{}
		return a.Client.List(ctx, smith.TemplateResourceGroupVersion, smith.AllNamespaces, smith.TemplateResourcePath, nil, nil, &templateList)
	}, func(e error) bool {
		// TODO be smarter about what is retried
		log.Printf("Failed to list resources %s: %v", smith.TemplateResourceName, e)
		return false
	})
	if err != nil {
		return err
	}

	// 6. Start rebuilds for existing templates to re-assert their state
	for _, template := range templateList.Items {
		a.Processor.Rebuild(template)
	}

	// 7. Watch for template-related events
	a.Watcher.Watch(smith.TemplateResourceGroupVersion, smith.AllNamespaces, smith.TemplateResourcePath, templateList.ResourceVersion, newTemplateEvent)

	// 8. Process events and trigger rebuilds as necessary
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-a.Events:
			a.handleEvent(event)
		}
	}
}

func (a *App) handleEvent(event interface{}) {
	switch ev := event.(type) {
	case error:
		log.Printf("Something went wrong with watch: %v", ev)
	case *smith.TemplateWatchEvent:
		switch ev.Type {
		case smith.Added, smith.Modified:
			a.Processor.Rebuild(*ev.Object)
		case smith.Deleted:
		// TODO Somehow use finalizers to prevent direct deletion?
		// "No direct deletion" convention? Use ObjectMeta.DeletionTimestamp like Namespace does?
		// Somehow implement GC to do cleanup after template is deleted?
		// Maybe store template in annotation on each resource to help reconstruct the dependency graph for GC?
		case smith.Error:
			// TODO what to do with it?
			log.Printf("Watch returned an Error event: %#v", ev)
		}
	case *smith.TprInstanceWatchEvent:
		switch ev.Type {
		case smith.Added, smith.Modified, smith.Deleted:
			templateName := ev.Object.Labels[smith.TemplateNameLabel]
			if templateName != "" {
				a.Processor.RebuildByName(ev.Object.Namespace, templateName)
			}
		case smith.Error:
			// TODO what to do with it?
			log.Printf("Watch returned an Error event: %#v", ev)
		}
	case *smith.TprWatchEvent:
		switch ev.Type {
		case smith.Added:
			a.watchTpr(ev.Object)
		// TODO rebuild all templates containing resources of this type
		case smith.Modified:
			a.forgetTpr(ev.Object)
			a.watchTpr(ev.Object)
		// TODO rebuild all templates containing resources of this type
		case smith.Deleted:
			a.forgetTpr(ev.Object)
		// TODO rebuild all templates containing resources of this type
		case smith.Error:
			// TODO what to do with it?
			log.Printf("Watch returned an Error event: %#v", ev)
		}
	default:
		log.Printf("Unexpected event type: %T", event)
	}
}

func newTemplateEvent() interface{} {
	return &smith.TemplateWatchEvent{}
}

func newTprInstanceEvent() interface{} {
	return &smith.TprInstanceWatchEvent{}
}

func newTprEvent() interface{} {
	return &smith.TprWatchEvent{}
}

func ensureResourceExists(ctx context.Context, c *client.ResourceClient) error {
	log.Printf("Creating ThirdPartyResource %s", smith.TemplateResourceName)
	res := &smith.ThirdPartyResource{}
	err := c.Create(ctx, smith.ThirdPartyResourceGroupVersion, "", "thirdpartyresources", &smith.ThirdPartyResource{
		TypeMeta: smith.TypeMeta{
			Kind:       "ThirdPartyResource",
			APIVersion: smith.ThirdPartyResourceGroupVersion,
		},
		ObjectMeta: smith.ObjectMeta{
			Name: smith.TemplateResourceName,
		},
		Description: "Smith resource manager",
		Versions: []smith.APIVersion{
			{Name: smith.TemplateResourceVersion},
		},
	}, res)
	if err != nil {
		log.Printf("%#v", err)
		if !client.IsConflict(err) {
			return fmt.Errorf("failed to create ThirdPartyResource: %v", err)
		}
		log.Printf("ThirdPartyResource %s already exists", smith.TemplateResourceName)
	} else {
		log.Printf("ThirdPartyResource %s created: %+v", smith.TemplateResourceName, res)
	}
	return nil
}

func (a *App) watchTpr(tpr *smith.ThirdPartyResource) {
	// TODO only watch supported TPRs (inspect annotations?)
	path, group := splitTprName(tpr.Name)
	for _, version := range tpr.Versions {
		a.Watcher.Watch(group+"/"+version.Name, smith.AllNamespaces, path, "", newTprInstanceEvent)
	}
}

func (a *App) forgetTpr(tpr *smith.ThirdPartyResource) {
	path, group := splitTprName(tpr.Name)
	for _, version := range tpr.Versions {
		a.Watcher.Forget(group+"/"+version.Name, smith.AllNamespaces, path)
	}
}

// splitTprName splits TPR's name into resource name and group name.
// e.g. "postgresql-resource.smith-sql.ash2k.com" is split into "postgresqlresources" and "smith-sql.ash2k.com".
// See https://github.com/kubernetes/kubernetes/blob/master/docs/design/extending-api.md
// See k8s.io/pkg/api/meta/restmapper.go:147 KindToResource()
func splitTprName(name string) (string, string) {
	pos := strings.IndexByte(name, '.')
	if pos == -1 || pos == 0 {
		panic(fmt.Errorf("invalid resource name: %q", name))
	}
	resourcePath := strings.Replace(name[:pos], "-", "", -1)
	switch string(resourcePath[len(resourcePath)-1]) {
	case "s":
		resourcePath += "es"
	case "y":
		resourcePath = resourcePath[:len(resourcePath)-1] + "ies"
	default:
		resourcePath += "s"
	}
	return resourcePath, name[pos+1:]
}
