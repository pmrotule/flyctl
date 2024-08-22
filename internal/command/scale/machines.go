package scale

import (
	"context"
	"fmt"
	"os"

	"github.com/samber/lo"
	fly "github.com/superfly/fly-go"
	"github.com/superfly/fly-go/flaps"
	"github.com/superfly/flyctl/internal/appconfig"
	"github.com/superfly/flyctl/internal/flapsutil"
	mach "github.com/superfly/flyctl/internal/machine"
)

func v2ScaleVM(ctx context.Context, appName, group, sizeName string, memoryMB int) (*fly.VMSize, error) {
	flapsClient, err := flapsutil.NewClientWithOptions(ctx, flaps.NewClientOpts{
		AppName: appName,
	})
	if err != nil {
		return nil, err
	}
	fmt.Println("Size Name")
	fmt.Println(sizeName)
	ctx = flapsutil.NewContextWithClient(ctx, flapsClient)

	// Quickly validate sizeName before any network call
	if err := (&fly.MachineGuest{}).SetSize(sizeName); err != nil && sizeName != "" {
		return nil, err
	}

	if group == "" {
		appConfig, err := appconfig.FromRemoteApp(ctx, appName)
		if err != nil {
			return nil, err
		}
		if len(appConfig.Processes) > 1 {
			return nil, fmt.Errorf("scaling an app with multiple process groups requires specifying a group with '--process-group <name>'\n * this app has the following process groups: %v", appConfig.FormatProcessNames())
		}
		group = appConfig.DefaultProcessName()
	}

	machines, err := listMachinesWithGroup(ctx, group)
	if err != nil {
		return nil, err
	}
	if len(machines) == 0 {
		return nil, fmt.Errorf("No active machines in process group '%s', check `fly status` output", group)
	}

	machines, releaseFunc, err := mach.AcquireLeases(ctx, machines)
	defer releaseFunc()
	if err != nil {
		return nil, err
	}

	for _, machine := range machines {

		fmt.Println(machine)
		
		if sizeName != "" {
			machine.Config.Guest.SetSize(sizeName)
		}
		if memoryMB > 0 {
			machine.Config.Guest.MemoryMB = memoryMB
		}

		input := &fly.LaunchMachineInput{
			Name:   machine.Name,
			Region: machine.Region,
			Config: machine.Config,
		}
		
		input.Config.Guest.CPUs = 2
		fmt.Println(input.Config.Guest.CPUs)
		os.Exit(1)

		//fmt.Println("testing retry...")
		if err := mach.Update(ctx, machine, input); err != nil {
			_, fix_err := err.(mach.InvalidConfigErr).AttemptFix() 
			fmt.Println(fix_err)
			if fix_err!=nil{
				fmt.Println("unable to succeed!")
			}else{
				fmt.Println("retrying scale memory...")
				// Retry the updatre now
				if second_err := mach.Update(ctx, machine, input); second_err != nil {
					return nil, second_err
				}
			}
			return nil, err
		}
		
	}

	// Return fly.VMSize to remain compatible with v1 scale app signature
	size := &fly.VMSize{
		Name:     machines[0].Config.Guest.ToSize(),
		MemoryMB: machines[0].Config.Guest.MemoryMB,
		CPUCores: float32(machines[0].Config.Guest.CPUs),
	}

	return size, nil
}

func listMachinesWithGroup(ctx context.Context, group string) ([]*fly.Machine, error) {
	machines, err := mach.ListActive(ctx)

	fmt.Println( machines )
	if err != nil {
		return nil, err
	}

	machines = lo.Filter(machines, func(m *fly.Machine, _ int) bool {
		return m.ProcessGroup() == group
	})

	return machines, nil
}
