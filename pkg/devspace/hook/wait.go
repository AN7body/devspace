package hook

import (
	"context"
	"github.com/loft-sh/devspace/pkg/devspace/config"
	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	"github.com/loft-sh/devspace/pkg/devspace/dependency/types"
	"github.com/loft-sh/devspace/pkg/devspace/deploy/deployer/util"
	"github.com/loft-sh/devspace/pkg/devspace/kubectl"
	"github.com/loft-sh/devspace/pkg/devspace/services/targetselector"
	"github.com/loft-sh/devspace/pkg/util/imageselector"
	logpkg "github.com/loft-sh/devspace/pkg/util/log"
	"github.com/mgutz/ansi"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"sync"
	"time"
)

func NewWaitHook() Hook {
	return &waitHook{}
}

type waitHook struct {
	printWarning sync.Once
}

func (r *waitHook) Execute(ctx Context, hook *latest.HookConfig, config config.Config, dependencies []types.Dependency, log logpkg.Logger) error {
	if ctx.Client == nil {
		return errors.Errorf("Cannot execute hook '%s': kube client is not initialized", ansi.Color(hookName(hook), "white+b"))
	}

	var (
		imageSelectors []imageselector.ImageSelector
		err            error
	)
	if hook.Where.Container.ImageName != "" || hook.Where.Container.ImageSelector != "" {
		if config == nil || config.Generated() == nil {
			return errors.Errorf("Cannot execute hook '%s': config is not loaded", ansi.Color(hookName(hook), "white+b"))
		}

		if hook.Where.Container.ImageName != "" {
			imageSelectorFromConfig, err := imageselector.Resolve(hook.Where.Container.ImageName, config, dependencies)
			if err != nil {
				return err
			}
			if imageSelectorFromConfig != nil {
				imageSelectors = append(imageSelectors, *imageSelectorFromConfig)
			}
		}

		if hook.Where.Container.ImageSelector != "" {
			imageSelector, err := util.ResolveImageAsImageSelector(hook.Where.Container.ImageSelector, config, dependencies)
			if err != nil {
				return err
			}

			imageSelectors = append(imageSelectors, *imageSelector)
		}
	}

	err = r.execute(ctx, hook, imageSelectors, log)
	if err != nil {
		return err
	}

	log.Donef("Hook '%s' successfully executed", ansi.Color(hookName(hook), "white+b"))
	return nil
}

func (r *waitHook) execute(ctx Context, hook *latest.HookConfig, imageSelector []imageselector.ImageSelector, log logpkg.Logger) error {
	labelSelector := ""
	if len(hook.Where.Container.LabelSelector) > 0 {
		labelSelector = labels.Set(hook.Where.Container.LabelSelector).String()
	}

	timeout := int64(150)
	if hook.Wait.Timeout > 0 {
		timeout = hook.Wait.Timeout
	}

	// wait until the defined condition will be true, this will wait initially 2 seconds
	err := wait.Poll(time.Second*2, time.Duration(timeout)*time.Second, func() (done bool, err error) {
		podContainers, err := kubectl.NewFilter(ctx.Client).SelectContainers(context.TODO(), kubectl.Selector{
			ImageSelector: imageSelector,
			LabelSelector: labelSelector,
			Pod:           hook.Where.Container.Pod,
			ContainerName: hook.Where.Container.ContainerName,
			Namespace:     hook.Where.Container.Namespace,
		})
		if err != nil {
			return false, err
		}

		// lets check if all containers satisfy the condition
		for _, pc := range podContainers {
			if targetselector.HasPodProblem(pc.Pod) {
				r.printWarning.Do(func() {
					status := kubectl.GetPodStatus(pc.Pod)
					log.Warnf("Pod %s/%s has critical status: %s. DevSpace will continue waiting, but this operation might timeout", pc.Pod.Namespace, pc.Pod.Name, status)
				})
			}

			if isWaitConditionTrue(hook.Wait, pc) == false {
				return false, nil
			}
		}

		return true, nil
	})
	if err != nil {
		return err
	}

	return nil
}

func isWaitConditionTrue(condition *latest.HookWaitConfig, podContainer *kubectl.SelectedPodContainer) bool {
	if podContainer.Pod.DeletionTimestamp != nil {
		return false
	}

	for _, cs := range podContainer.Pod.Status.InitContainerStatuses {
		if cs.Name == podContainer.Container.Name {
			if condition.Running && cs.State.Running != nil && cs.Ready {
				return true
			}
			if condition.TerminatedWithCode != nil && cs.State.Terminated != nil && cs.State.Terminated.ExitCode == *condition.TerminatedWithCode {
				return true
			}
		}
	}
	for _, cs := range podContainer.Pod.Status.ContainerStatuses {
		if cs.Name == podContainer.Container.Name {
			if condition.Running && cs.State.Running != nil && cs.Ready {
				return true
			}
			if condition.TerminatedWithCode != nil && cs.State.Terminated != nil && cs.State.Terminated.ExitCode == *condition.TerminatedWithCode {
				return true
			}
		}
	}

	return false
}
