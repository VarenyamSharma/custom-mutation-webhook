package main

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// test is a simple helper function to show how to use the Kubernetes client.
// It fetches and counts the total number of Pods running in the entire cluster.
func test() {

	// Fetch a list of all Pods across all namespaces.
	// The "" means "all namespaces". context.TODO() is used as an empty placeholder context.
	pods, err := clientSet.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})

	// If there's an error (e.g., it can't connect to the cluster), stop the program and show the error.
	if err != nil {
		panic(err.Error())
	}

	// Print out the total number of Pods found.
	fmt.Printf("There are %d pods in the cluster\n", len(pods.Items))
}
