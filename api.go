package docker

import (
	_ "bytes"
	"encoding/json"
	"fmt"
	"github.com/gorilla/mux"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func ListenAndServe(addr string, rtime *Runtime) error {
	r := mux.NewRouter()
	log.Printf("Listening for HTTP on %s\n", addr)

	r.Path("/version").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		m := ApiVersion{VERSION, GIT_COMMIT, NO_MEMORY_LIMIT}
		b, err := json.Marshal(m)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			w.Write(b)
		}
	})

	r.Path("/containers/{name:.*}/kill").Methods("POST").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]
		if container := rtime.Get(name); container != nil {
			if err := container.Kill(); err != nil {
				http.Error(w, "Error restarting container "+name+": "+err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			http.Error(w, "No such container: "+name, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	r.Path("/containers/{name:.*}/export").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]

		if container := rtime.Get(name); container != nil {

			data, err := container.Export()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer conn.Close()
			file, err := conn.(*net.TCPConn).File()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer file.Close()

			fmt.Fprintln(file, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\n\r\n")
			// Stream the entire contents of the container (basically a volatile snapshot)
			if _, err := io.Copy(file, data); err != nil {
				fmt.Fprintln(file, "Error: "+err.Error())
				return
			}
		} else {
			http.Error(w, "No such container: "+name, http.StatusNotFound)
		}

	})

	r.Path("/images").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		All := r.Form.Get("all")
		NameFilter := r.Form.Get("filter")
		Quiet := r.Form.Get("quiet")

		var allImages map[string]*Image
		var err error
		if All == "1" {
			allImages, err = rtime.graph.Map()
		} else {
			allImages, err = rtime.graph.Heads()
		}
		if err != nil {
			w.WriteHeader(500)
			return
		}
		var outs []ApiImages
		for name, repository := range rtime.repositories.Repositories {
			if NameFilter != "" && name != NameFilter {
				continue
			}
			for tag, id := range repository {
				var out ApiImages
				image, err := rtime.graph.Get(id)
				if err != nil {
					log.Printf("Warning: couldn't load %s from %s/%s: %s", id, name, tag, err)
					continue
				}
				delete(allImages, id)
				if Quiet != "1" {
					out.Repository = name
					out.Tag = tag
					out.Id = TruncateId(id)
					out.Created = HumanDuration(time.Now().Sub(image.Created)) + " ago"
				} else {
					out.Id = image.ShortId()
				}
				outs = append(outs, out)
			}
		}
		// Display images which aren't part of a
		if NameFilter == "" {
			for id, image := range allImages {
				var out ApiImages
				if Quiet != "1" {
					out.Repository = "<none>"
					out.Tag = "<none>"
					out.Id = TruncateId(id)
					out.Created = HumanDuration(time.Now().Sub(image.Created)) + " ago"
				} else {
					out.Id = image.ShortId()
				}
				outs = append(outs, out)
			}
		}

		b, err := json.Marshal(outs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			w.Write(b)
		}
	})

	r.Path("/info").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		images, _ := rtime.graph.All()
		var imgcount int
		if images == nil {
			imgcount = 0
		} else {
			imgcount = len(images)
		}
		var out ApiInfo
		out.Containers = len(rtime.List())
		out.Version = VERSION
		out.Images = imgcount
		if os.Getenv("DEBUG") == "1" {
			out.Debug = true
			out.NFd = getTotalUsedFds()
			out.NGoroutines = runtime.NumGoroutine()
		}
		b, err := json.Marshal(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			w.Write(b)
		}
	})

	r.Path("/images/{name:.*}/history").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]

		image, err := rtime.repositories.LookupImage(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var outs []ApiHistory
		err = image.WalkHistory(func(img *Image) error {
			var out ApiHistory
			out.Id = rtime.repositories.ImageName(img.ShortId())
			out.Created = HumanDuration(time.Now().Sub(img.Created)) + " ago"
			out.CreatedBy = strings.Join(img.ContainerConfig.Cmd, " ")
			return nil
		})

		b, err := json.Marshal(outs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			w.Write(b)
		}
	})

	r.Path("/containers/{name:.*}/logs").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]

		if container := rtime.Get(name); container != nil {
			var out ApiLogs

			logStdout, err := container.ReadLog("stdout")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			logStderr, err := container.ReadLog("stderr")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			stdout, errStdout := ioutil.ReadAll(logStdout)
			if errStdout != nil {
				http.Error(w, errStdout.Error(), http.StatusInternalServerError)
				return
			} else {
				out.Stdout = fmt.Sprintf("%s", stdout)
			}
			stderr, errStderr := ioutil.ReadAll(logStderr)
			if errStderr != nil {
				http.Error(w, errStderr.Error(), http.StatusInternalServerError)
				return
			} else {
				out.Stderr = fmt.Sprintf("%s", stderr)
			}

			b, err := json.Marshal(out)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			} else {
				w.Write(b)
			}

		} else {
			http.Error(w, "No such container: "+name, http.StatusNotFound)
		}
	})

	r.Path("/containers/{name:.*}/changes").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]

		if container := rtime.Get(name); container != nil {
			changes, err := container.Changes()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			var changesStr []string
			for _, name := range changes {
				changesStr = append(changesStr, name.String())
			}
			b, err := json.Marshal(changesStr)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			} else {
				w.Write(b)
			}
		} else {
			http.Error(w, "No such container: "+name, http.StatusNotFound)
		}
	})

	r.Path("/containers/{name:.*}/port").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		privatePort := r.Form.Get("port")
		vars := mux.Vars(r)
		name := vars["name"]

		if container := rtime.Get(name); container == nil {
			http.Error(w, "No such container: "+name, http.StatusNotFound)
			return
		} else {
			if frontend, exists := container.NetworkSettings.PortMapping[privatePort]; !exists {
				http.Error(w, "No private port '"+privatePort+"' allocated on "+name, http.StatusInternalServerError)
				return
			} else {
				b, err := json.Marshal(ApiPort{frontend})
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				} else {
					w.Write(b)
				}
			}
		}
	})

	r.Path("/containers").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		All := r.Form.Get("all")
		NoTrunc := r.Form.Get("notrunc")
		Quiet := r.Form.Get("quiet")
		Last := r.Form.Get("n")
		n, err := strconv.Atoi(Last)
		if err != nil {
			n = -1
		}
		var outs []ApiContainers

		for i, container := range rtime.List() {
			if !container.State.Running && All != "1" && n == -1 {
				continue
			}
			if i == n {
				break
			}
			var out ApiContainers
			out.Id = container.ShortId()
			if Quiet != "1" {
				command := fmt.Sprintf("%s %s", container.Path, strings.Join(container.Args, " "))
				if NoTrunc != "1" {
					command = Trunc(command, 20)
				}
				out.Image = rtime.repositories.ImageName(container.Image)
				out.Command = command
				out.Created = HumanDuration(time.Now().Sub(container.Created)) + " ago"
				out.Status = container.State.String()
			}
			outs = append(outs, out)
		}

		b, err := json.Marshal(outs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			w.Write(b)
		}
	})

	r.Path("/containers/{name:.*}/commit").Methods("POST").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		vars := mux.Vars(r)
		name := vars["name"]
		repo := r.Form.Get("repo")
		tag := r.Form.Get("tag")
		comment := r.Form.Get("comment")

		img, err := rtime.Commit(name, repo, tag, comment)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		b, err := json.Marshal(ApiCommit{img.ShortId()})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			w.Write(b)
		}
	})

	r.Path("/images/{name:.*}/tag").Methods("POST").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		vars := mux.Vars(r)
		name := vars["name"]
		repo := r.Form.Get("repo")
		tag := r.Form.Get("tag")
		var force bool
		if r.Form.Get("force") == "1" {
			force = true
		}

		if err := rtime.repositories.Set(repo, tag, name, force); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})

	r.Path("/images/{name:.*}/pull").Methods("POST").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]

		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		file, err := conn.(*net.TCPConn).File()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer file.Close()

		fmt.Fprintln(file, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n")
		if rtime.graph.LookupRemoteImage(name, rtime.authConfig) {
			if err := rtime.graph.PullImage(file, name, rtime.authConfig); err != nil {
				fmt.Fprintln(file, "Error: "+err.Error())
			}
			return
		}
		if err := rtime.graph.PullRepository(file, name, "", rtime.repositories, rtime.authConfig); err != nil {
			fmt.Fprintln(file, "Error: "+err.Error())
		}
	})

	r.Path("/containers").Methods("POST").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		var config Config
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		file, err := conn.(*net.TCPConn).File()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer file.Close()

		if config.Tty {
			oldState, err := SetRawTerminal()
			if err != nil {
				if os.Getenv("DEBUG") != "" {
					log.Printf("Can't set the terminal in raw mode: %s", err)
				}
			} else {
				defer RestoreTerminal(oldState)
			}
		}

		// Flush the options to make sure the client sets the raw mode
		// or tell the client there is no options
		conn.Write([]byte{})

		fmt.Fprintln(file, "HTTP/1.1 201 Created\r\nContent-Type: application/json\r\n\r\n")
		container, err := rtime.Create(&config)
		if err != nil {
			// If container not found, try to pull it
			if rtime.graph.IsNotExist(err) {
				fmt.Fprintf(file, "Image %s not found, trying to pull it from registry.\r\n", config.Image)
				if rtime.graph.LookupRemoteImage(config.Image, rtime.authConfig) {
					if err := rtime.graph.PullImage(file, config.Image, rtime.authConfig); err != nil {
						fmt.Fprintln(file, "Error: "+err.Error())
						return
					}
				} else if err := rtime.graph.PullRepository(file, config.Image, "", rtime.repositories, rtime.authConfig); err != nil {
					fmt.Fprintln(file, "Error: "+err.Error())
					return
				}
				if container, err = rtime.Create(&config); err != nil {
					fmt.Fprintln(file, "Error: "+err.Error())
					return
				}
			} else {
				fmt.Fprintln(file, "Error: "+err.Error())
				return
			}
		}
		var (
			cStdin           io.ReadCloser
			cStdout, cStderr io.Writer
		)
		if config.AttachStdin {
			r, w := io.Pipe()
			go func() {
				defer w.Close()
				defer Debugf("Closing buffered stdin pipe")
				io.Copy(w, file)
			}()
			cStdin = r
		}
		if config.AttachStdout {
			cStdout = file
		}
		if config.AttachStderr {
			cStderr = file // FIXME: api can't differentiate stdout from stderr
		}

		attachErr := container.Attach(cStdin, file, cStdout, cStderr)
		Debugf("Starting\n")
		if err := container.Start(); err != nil {
			fmt.Fprintln(file, "Error: "+err.Error())
			return
		}
		if cStdout == nil && cStderr == nil {
			fmt.Fprintln(file, container.ShortId())
		}
		Debugf("Waiting for attach to return\n")
		<-attachErr
		// Expecting I/O pipe error, discarding

		// If we are in stdinonce mode, wait for the process to end
		// otherwise, simply return
		if config.StdinOnce && !config.Tty {
			container.Wait()
		}
	})

	r.Path("/containers/{name:.*}/attach").Methods("POST").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]
		if container := rtime.Get(name); container != nil {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer conn.Close()
			file, err := conn.(*net.TCPConn).File()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer file.Close()

			if container.Config.Tty {
				oldState, err := SetRawTerminal()
				if err != nil {
					if os.Getenv("DEBUG") != "" {
						log.Printf("Can't set the terminal in raw mode: %s", err)
					}
				} else {
					defer RestoreTerminal(oldState)
				}

			}

			// Flush the options to make sure the client sets the raw mode
			conn.Write([]byte{})

			fmt.Fprintln(file, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n")
			r, w := io.Pipe()
			go func() {
				defer w.Close()
				defer Debugf("Closing buffered stdin pipe")
				io.Copy(w, file)
			}()
			cStdin := r

			<-container.Attach(cStdin, nil, file, file)
			// Expecting I/O pipe error, discarding                     

		} else {
			http.Error(w, "No such container: "+name, http.StatusNotFound)
			return
		}
	})

	r.Path("/containers/{name:.*}/restart").Methods("POST").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]
		if container := rtime.Get(name); container != nil {
			if err := container.Restart(); err != nil {
				http.Error(w, "Error restarting container "+name+": "+err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			http.Error(w, "No such container: "+name, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	r.Path("/containers/{name:.*}").Methods("DELETE").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]
		if container := rtime.Get(name); container != nil {
			if err := rtime.Destroy(container); err != nil {
				http.Error(w, "Error destroying container "+name+": "+err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			http.Error(w, "No such container: "+name, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	r.Path("/images/{name:.*}").Methods("DELETE").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]

		img, err := rtime.repositories.LookupImage(name)
		if err != nil {
			http.Error(w, "No such image: "+name, http.StatusNotFound)
			return
		} else {
			if err := rtime.graph.Delete(img.Id); err != nil {
				http.Error(w, "Error deleting image "+name+": "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})

	r.Path("/containers/{name:.*}/start").Methods("POST").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]
		if container := rtime.Get(name); container != nil {
			if err := container.Start(); err != nil {
				http.Error(w, "Error starting container "+name+": "+err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			http.Error(w, "No such container: "+name, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	r.Path("/containers/{name:.*}/stop").Methods("POST").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]
		if container := rtime.Get(name); container != nil {
			if err := container.Stop(); err != nil {
				http.Error(w, "Error stopping container "+name+": "+err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			http.Error(w, "No such container: "+name, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	r.Path("/containers/{name:.*}/wait").Methods("POST").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]
		if container := rtime.Get(name); container != nil {
			b, err := json.Marshal(ApiWait{container.Wait()})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			} else {
				w.Write(b)
			}
			return
		} else {
			http.Error(w, "No such container: "+name, http.StatusNotFound)
			return
		}
	})

	r.Path("/containers/{name:.*}").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]

		if container := rtime.Get(name); container != nil {
			b, err := json.Marshal(container)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			} else {
				w.Write(b)
			}
			return
		}
		http.Error(w, "No such container: "+name, http.StatusNotFound)
	})

	r.Path("/images/{name:.*}").Methods("GET").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.RequestURI)
		vars := mux.Vars(r)
		name := vars["name"]

		if image, err := rtime.repositories.LookupImage(name); err == nil && image != nil {
			b, err := json.Marshal(image)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			} else {
				w.Write(b)
			}
			return
		}
		http.Error(w, "No such image: "+name, http.StatusNotFound)
	})

	return http.ListenAndServe(addr, r)
}