/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	goipam "github.com/metal-stack/go-ipam"
	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	vpcv1alpha1 "github.com/Sh4d1/scaleway-k8s-vpc/api/v1alpha1"
	"github.com/Sh4d1/scaleway-k8s-vpc/internal/constants"
)

// NetworkInterfaceReconciler reconciles a NetworkInterface object
type NetworkInterfaceReconciler struct {
	client.Client
	Log         logr.Logger
	Scheme      *runtime.Scheme
	IPAM        goipam.Ipamer
	InstanceAPI *instance.API
}

// +kubebuilder:rbac:groups=vpc.scaleway.com,resources=networkinterfaces,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=vpc.scaleway.com,resources=networkinterfaces/status,verbs=get;patch
// +kubebuilder:rbac:groups=vpc.scaleway.com,resources=privatenetworks,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

func (r *NetworkInterfaceReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("networkinterface", req.NamespacedName)

	nic := &vpcv1alpha1.NetworkInterface{}

	log.Info("Getting network interface")
	err := r.Client.Get(ctx, req.NamespacedName, nic)
	if err != nil {
		log.Error(err, "could not find object")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info(fmt.Sprintf("Got network interface %+v", nic.Status))

	node := corev1.Node{}
	err = r.Client.Get(ctx, types.NamespacedName{Name: nic.Spec.NodeName}, &node)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "error getting node")
		return ctrl.Result{}, err
	}

	nodeDeleted := err != nil && apierrors.IsNotFound(err)

	pn := vpcv1alpha1.PrivateNetwork{}
	err = r.Client.Get(ctx, types.NamespacedName{Name: nic.OwnerReferences[0].Name}, &pn)
	if err != nil {
		log.Error(err, "unable to get private network")
		return ctrl.Result{}, err
	}

	if nic.ObjectMeta.GetDeletionTimestamp().IsZero() {
		if nodeDeleted {
			err := r.Client.Delete(ctx, nic)
			if err != nil {
				log.Error(err, fmt.Sprintf("failed to delete networkInterface %s", nic.Name))
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}
		if len(nic.Status.Address) == 0 && pn.Spec.IPAM != nil {
			switch pn.Spec.IPAM.Type {
			case vpcv1alpha1.IPAMTypeDHCP:
				// this case is handled in the node controller
			case vpcv1alpha1.IPAMTypeStatic:
				if pn.Spec.IPAM.Static == nil {
					return ctrl.Result{}, fmt.Errorf("Static CIDR can't be empty on static ipam mode")
				}
				cidrs := []string{pn.Spec.IPAM.Static.CIDR}
				if len(pn.Spec.IPAM.Static.AvailableRanges) != 0 {
					cidrs = pn.Spec.IPAM.Static.AvailableRanges
				}

				var ip *goipam.IP
				var chosenCidr string

				for _, cidr := range cidrs {
					prefix, err := r.IPAM.NewPrefix(cidr)
					if err != nil {
						log.Error(err, "error creating new prefix")
						continue
					}
					log.Info("AcquireIP")
					ip, err = r.IPAM.AcquireIP(prefix.Cidr)
					if err != nil {
						log.Error(err, fmt.Sprintf("error acquiring ip for cidr %s", prefix.Cidr))
						continue
					}
					log.Info(fmt.Sprintf("got IP %+v", ip))
					chosenCidr = prefix.Cidr
					break
				}

				if ip == nil {
					err := fmt.Errorf("could not acquire IP")
					log.Error(err, "error while testing all cidrs")
					return ctrl.Result{RequeueAfter: RequeueDuration}, err
				}

				// TODO have a better idea :D
				patch := client.MergeFrom(nic.DeepCopy())
				nic.Status.Address = ip.IP.String() + "/" + strings.Split(pn.Spec.IPAM.Static.CIDR, "/")[1]
				nic.Status.ParentCIDR = chosenCidr
				log.Info(fmt.Sprintf("Patching IP + CIDR %+v", ip))
				change, _ := patch.Data(nic)
				log.Info(fmt.Sprintf("%s", change))
				err = r.Client.Status().Patch(ctx, nic, patch)
				if err != nil {
					ipamErr := r.IPAM.ReleaseIPFromPrefix(chosenCidr, strings.Split(nic.Status.Address, "/")[0])
					if ipamErr != nil {
						log.Error(ipamErr, fmt.Sprintf("failed to release IP %s", nic.Status.Address))
					}
					log.Error(err, fmt.Sprintf("failed to update networkInterface %s", nic.Name))
					return ctrl.Result{}, err
				}
				log.Info(fmt.Sprintf("Patched IP + CIDR %+v %+v", ip, nic.Status))

				log.Info("Double checking network interface..")
				err := r.Client.Get(ctx, req.NamespacedName, nic)
				if err != nil {
					log.Error(err, "could not find object")
					return ctrl.Result{}, client.IgnoreNotFound(err)
				}

				if len(nic.Status.Address) == 0 {
					log.Info("Releasing IP as it was not properly patched.")

					ipamErr := r.IPAM.ReleaseIPFromPrefix(chosenCidr, ip.IP.String())
					if ipamErr != nil {
						log.Error(ipamErr, fmt.Sprintf("failed to release IP %s", ip.IP.String()))
					}
					return ctrl.Result{}, fmt.Errorf("failed to assign IP to %s", nic.Name)
				}

				log.Info(fmt.Sprintf("Double checked network interface to be %+v", nic.Status))
			default:
				return ctrl.Result{}, fmt.Errorf("IPAM type %s is not supported", pn.Spec.IPAM.Type)
			}
		}
		// nothing left to do
		return ctrl.Result{}, nil
	}

	// nic is deleting

	if controllerutil.ContainsFinalizer(nic, constants.FinalizerName) && nodeDeleted {
		patch := client.MergeFrom(nic.DeepCopy())
		controllerutil.RemoveFinalizer(nic, constants.FinalizerName)
		err = r.Client.Patch(ctx, nic, patch)
		if err != nil {
			log.Error(err, fmt.Sprintf("failed to patch networkInterface %s", nic.Name))
			return ctrl.Result{}, err
		}
		//return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	r.Log.Info("Checking if Finalizer is present..")

	if !controllerutil.ContainsFinalizer(nic, constants.FinalizerName) {
		r.Log.Info("Finalizer not present on nic. Removing.")
		r.Log.Info(fmt.Sprintf("%+v %+v", pn.Spec.IPAM, pn.Spec.IPAM.Type))

		if pn.Spec.IPAM != nil && pn.Spec.IPAM.Type == vpcv1alpha1.IPAMTypeStatic {
			r.Log.Info("Going into releasing IPv4 address..")

			if pn.Spec.IPAM.Static == nil {
				return ctrl.Result{}, fmt.Errorf("Static CIDR can't be empty on static ipam mode")
			}

			cidr := pn.Spec.IPAM.Static.CIDR
			if nic.Status.ParentCIDR != "" {
				cidr = nic.Status.ParentCIDR
			}
			r.Log.Info("Releasing IPv4 address..")
			r.Log.Info(fmt.Sprintf("%+v %+v", cidr, nic.Status.Address))
			err := r.IPAM.ReleaseIPFromPrefix(cidr, strings.Split(nic.Status.Address, "/")[0])
			if err != nil {
				r.Log.Info("Error while releasing IPv4 address..")
				if !errors.As(err, &goipam.NotFoundError{}) {
					log.Error(err, fmt.Sprintf("could not delete IP %s from prefix %s", nic.Status.Address, cidr))
					return ctrl.Result{}, err
				}
			}
			r.Log.Info("Finished releasing IPv4 address..")
		}
		r.Log.Info("Continuing code after releasing IPv4 address..")
		node := corev1.Node{}
		err := r.Client.Get(ctx, types.NamespacedName{Name: nic.Spec.NodeName}, &node)
		if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "error getting node")
			return ctrl.Result{}, err
		}
		if err == nil {
			server, err := getServerFromNode(r.InstanceAPI, &node)
			if err != nil {
				log.Error(err, "error getting server from node")
				return ctrl.Result{}, err
			}
			privateNicID := ""
			for _, pnic := range server.PrivateNics {
				if pnic.PrivateNetworkID == pn.Spec.ID {
					privateNicID = pnic.ID
					break
				}
			}
			if privateNicID != "" {
				err := r.InstanceAPI.DeletePrivateNIC(&instance.DeletePrivateNICRequest{
					Zone:         server.Zone,
					PrivateNicID: privateNicID,
					ServerID:     server.ID,
				})
				if err != nil {
					log.Error(err, "unable to delete private nic from server")
					return ctrl.Result{}, err
				}
			}
		}

		patch := client.MergeFrom(nic.DeepCopy())
		controllerutil.RemoveFinalizer(nic, constants.IPFinalizerName)
		err = r.Client.Patch(ctx, nic, patch)
		if err != nil {
			log.Error(err, fmt.Sprintf("failed to remove finalizer on networkInterface %s", nic.Name))
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *NetworkInterfaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vpcv1alpha1.NetworkInterface{}).
		Watches(&source.Kind{
			Type: &corev1.Node{},
		}, &handler.Funcs{
			DeleteFunc: func(e event.DeleteEvent, q workqueue.RateLimitingInterface) {
				nicsList := &vpcv1alpha1.NetworkInterfaceList{}
				err := r.Client.List(context.Background(), nicsList,
					client.MatchingLabels{
						constants.NodeLabel: e.Meta.GetName(),
					},
				)
				if err != nil {
					r.Log.Error(err, "unable to sync privatenetwork on node creation")
					return
				}
				for _, nic := range nicsList.Items {
					q.Add(reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name: nic.Name,
						},
					})
				}
			},
		}).
		Complete(r)
}
