package main

import (
	"fmt"
	"os"
	"os/exec"
)

func serviceAction(action string) error {
	out, err := exec.Command("sc.exe", action, "ephemerd").CombinedOutput()
	if err != nil {
		return fmt.Errorf("sc %s: %s", action, out)
	}
	fmt.Printf("ephemerd %sed\n", action)
	return nil
}

func serviceLogs(lines int, follow bool) error {
	args := []string{"qe", "Application",
		"/q:*[System[Provider[@Name='ephemerd']]]",
		fmt.Sprintf("/c:%d", lines),
		"/f:text", "/rd:true"}
	cmd := exec.Command("wevtutil.exe", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
