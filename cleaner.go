package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"sync"

	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/eks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/aws-iam-authenticator/pkg/token"
)

const (
	emptyString = ""
	contextName = "eks-cleaner"
)

var (
	cluster, namespaces      string
	profile, roleArn, region string
	days                     int
	sess                     *session.Session
	client                   *eks.EKS
	wg                       sync.WaitGroup
	ignoreNamespaces         = []string{"kube-system", "kube-public"}
)

type clusterCfg struct {
	Server                   string `json:"server"`
	CertificateAuthorityData string `json:"certificate-authority-data"`
}

type clusterWithName struct {
	Cluster clusterCfg `json:"cluster"`
	Name    string     `json:"name"`
}

type contextCfg struct {
	Cluster string `json:"cluster"`
	User    string `json:"user"`
}

type userCfg struct {
	Name string `json:"name"`
}

type contextWithName struct {
	Context contextCfg `json:"context"`
	Name    string     `json:"name"`
}

type kubeConfig struct {
	APIVersion     string            `json:"apiVersion"`
	Clusters       []clusterWithName `json:"clusters"`
	Contexts       []contextWithName `json:"contexts"`
	CurrentContext string            `json:"current-context,"`
	Kind           string            `json:"kind"`
	Users          []userCfg         `json:"users"`
}

func arrayContains(arr []string, s string) bool {
	for _, a := range arr {
		if a == s {
			return true
		}
	}
	return false
}

func getKubeClient() (*kubernetes.Clientset, error) {
	describeClusterOut, err := client.DescribeCluster(&eks.DescribeClusterInput{Name: aws.String(cluster)})
	if err != nil {
		log.Println("ERROR: failed to describe eks cluster", cluster)
		return nil, err
	}

	kubeCfgFile, err := ioutil.TempFile("", "kube-config-")
	if err != nil {
		log.Println("ERROR: failed to create temporary file for kube config")
		return nil, err
	}
	defer func() { _ = os.Remove(kubeCfgFile.Name()) }()

	kubeCfg := kubeConfig{
		APIVersion: "v1",
		Clusters: []clusterWithName{{
			Name: *describeClusterOut.Cluster.Arn,
			Cluster: clusterCfg{Server: *describeClusterOut.Cluster.Endpoint,
				CertificateAuthorityData: *describeClusterOut.Cluster.CertificateAuthority.Data},
		}},
		Contexts: []contextWithName{{
			Name:    contextName,
			Context: contextCfg{Cluster: *describeClusterOut.Cluster.Arn, User: contextName},
		}},
		CurrentContext: contextName,
		Kind:           "Config",
		Users: []userCfg{{
			Name: contextName,
		}},
	}
	kubeCfgJSON, err := json.Marshal(kubeCfg)
	if err != nil {
		log.Println("ERROR: failed to marshal kubernetes client config")
		return nil, err
	}

	err = ioutil.WriteFile(kubeCfgFile.Name(), kubeCfgJSON, 0777)
	if err != nil {
		log.Println("ERROR: failed to write kubernetes client config to file")
		return nil, err
	}

	cc, err := clientcmd.BuildConfigFromFlags("", kubeCfgFile.Name())
	if err != nil {
		log.Println("ERROR: failed to create kubernetes client config")
		return nil, err
	}

	gen, err := token.NewGenerator(true, true)
	if err != nil {
		log.Println("ERROR: failed to construct token generator")
		return nil, err
	}

	var tkn token.Token
	if profile != emptyString {
		os.Setenv("AWS_PROFILE", profile)
		tkn, err = gen.Get(cluster)
	} else {
		tkn, err = gen.GetWithRole(cluster, roleArn)
	}

	if err != nil {
		log.Println("ERROR: failed to get EKS token")
		return nil, err
	}

	cc.BearerToken = tkn.Token

	kc, err := kubernetes.NewForConfig(cc)
	if err != nil {
		log.Println("ERROR: failed to create kubernetes clientset")
		return nil, err
	}
	return kc, nil
}

func cleanup(kc *kubernetes.Clientset, namespace string) {
	defer wg.Done()
	log.Println("INFO: fetching the list of jobs for", namespace, "namespace")
	jobsList, err := kc.BatchV1().Jobs(namespace).List(metav1.ListOptions{})
	if err != nil {
		log.Println("ERROR: failed to list the jobs for", namespace)
		log.Println("ERROR:", err)
	}
	isValidTS := func(ts *metav1.Time) bool {
		jobTS := ts.Unix()
		compareTS := metav1.Now().AddDate(0, 0, -days).Unix()
		if jobTS < compareTS {
			return true
		}
		return false
	}

	var deletableJobs []string
	var t *metav1.Time
	for _, job := range jobsList.Items {
		// Not cleaning up the active jobs
		if job.Status.Succeeded == 1 {
			t = job.Status.CompletionTime
		} else if job.Status.Failed == 1 {
			t = job.Status.StartTime
		}
		if t != nil {
			if isValidTS(t) {
				deletableJobs = append(deletableJobs, job.Name)
			}
		}
	}
	if len(deletableJobs) > 0 {
		log.Println("INFO:", len(deletableJobs), "jobs can be deleted from", namespace, "namespace")
		propagationPolicy := metav1.DeletePropagationBackground
		for _, jobName := range deletableJobs {
			err := kc.BatchV1().Jobs(namespace).Delete(jobName, &metav1.DeleteOptions{
				PropagationPolicy: &propagationPolicy,
			})
			if err != nil {
				log.Println("ERROR: failed to delete job", jobName, "from namespace", namespace)
			} else {
				log.Println("INFO: deleted job", jobName, "from namespace", namespace)
			}
		}
	} else {
		log.Println("INFO: no jobs meet the cleanup criteria in", namespace, "namespace")
	}
}

func main() {
	flag.StringVar(&cluster, "cluster", "", "Name of the EKS Cluster (REQUIRED)")
	flag.StringVar(&profile, "profile", "", "Profile that allows access to EKS Cluster. Provide either 'profile' or 'role'")
	flag.StringVar(&roleArn, "role-arn", "", "IAM Role ARN that allows access to EKS Cluster. Provide either 'profile' or 'role'")
	flag.StringVar(&region, "region", "", "AWS region of the EKS Cluster")
	flag.StringVar(&namespaces, "namespaces", "", "Comma delimited string of namespaces enclosed within quotes")
	flag.IntVar(&days, "days", 14, "jobs older than this day count will be cleaned")
	flag.Parse()
	if cluster == emptyString {
		log.Fatalln("ERROR: argument `cluster` is required")
	}

	if profile != emptyString {
		sess = session.Must(session.NewSessionWithOptions(
			session.Options{
				Profile: profile,
				Config: aws.Config{
					Region: aws.String(region),
				},
				SharedConfigState: session.SharedConfigEnable,
			},
		))
		client = eks.New(sess)
	} else if roleArn != emptyString {
		sess = session.Must(session.NewSession())
		client = eks.New(sess, &aws.Config{Credentials: stscreds.NewCredentials(sess, roleArn)})
	} else {
		log.Fatalln("ERROR: either `profile` or `role` is required")
	}

	kc, err := getKubeClient()
	if err != nil {
		log.Fatalln("ERROR:", err)
	}

	var validNamespaces, namespaceList []string
	ns, err := kc.CoreV1().Namespaces().List(metav1.ListOptions{})
	if err != nil {
		log.Println("ERROR: failed to list the kubernetes namespaces")
		log.Fatalln("ERROR:", err)
	}
	for _, n := range ns.Items {
		if !arrayContains(ignoreNamespaces, n.Name) {
			validNamespaces = append(validNamespaces, n.Name)
		}
	}

	if namespaces != emptyString {
		for _, namespace := range strings.Split(namespaces, ",") {
			namespace = strings.TrimSpace(namespace)
			if arrayContains(validNamespaces, namespace) {
				namespaceList = append(namespaceList, namespace)
			}
		}
	} else {
		namespaceList = append(namespaceList, validNamespaces...)
	}
	if len(namespaceList) > 0 {
		log.Println("INFO: cleaning up for namespaces:", strings.Join(namespaceList, ","))

		for _, namespace := range namespaceList {
			wg.Add(1)
			go cleanup(kc, namespace)
		}
		wg.Wait()
	} else {
		log.Println("INFO: no matching namespaces found")
	}
}
