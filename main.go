package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/gousb"
	"github.com/kevmo314/go-uvc"
	"github.com/kevmo314/go-uvc/pkg/descriptors"
)

const udevrule = `# Bigscreen Bigeye
KERNEL=="hidraw*", SUBSYSTEM=="hidraw", ATTRS{idVendor}=="35bd", ATTRS{idProduct}=="0202", MODE="0660", GROUP="users", TAG+="uaccess"
SUBSYSTEM=="usb", ATTRS{idVendor}=="35bd", ATTRS{idProduct}=="0202", MODE="0660", GROUP="users", TAG+="uaccess"
`
const udevfilename = "99-bsb-cams.rules"

func getdevice() (device string) {
	ctx := gousb.NewContext()
	defer ctx.Close()
	dev, err := ctx.OpenDeviceWithVIDPID(0x35bd, 0x0202)
	user, _ := user.Current()
	if user.Username == "root" && !*sudo {
		log.Print("Running As Root Isnt Reccomended For Safety, Creating A UDEV Rule To Allow Rootless Access ")
		if _, err := os.Stat(udevfilename); err == nil {
			log.Print("File Already Exists")
		} else {
			err := os.WriteFile(udevfilename, []byte(udevrule), 0644)
			if err != nil {
				log.Fatalf("Could not Create File: %v", err)
			}
		}
		var response string
		log.Print("Would You Like The UDEV Rule Automatically Moved And Put In Place? (Y/N)")
		fmt.Scan(&response)
		switch strings.ToLower(response) {
		case "yes", "ye", "y":
			log.Printf("Would You Like The Rule Moved To The Userspace (/usr/lib/udev/rules.d/%v) Or The Linux OS Space (/etc/udev/rules.d/%v) (Good For Atomic Operating Systems) ", udevfilename)
			log.Print("0/Userspace    1/OS Space")
			fmt.Scan(&response)
			switch strings.ToLower(response) {
			case "0", "userspace":
				log.Printf("Putting File In /usr/lib/udev/rules.d/%v !!", udevfilename)
				err := os.Rename(udevfilename, "/usr/lib/udev/rules.d/"+udevfilename)
				if err != nil {
					log.Fatalf("Could Not Move File: %v", err)
				}
			case "1", "os space":
				log.Printf("Putting File In /etc/udev/rules.d/%v !!", udevfilename)
				err := os.Rename(udevfilename, "/etc/udev/rules.d/"+udevfilename)
				if err != nil {
					log.Fatalf("Could Not Move File: %v", err)
				}
			default:
				log.Fatal("Invalid Response")
			}
			log.Print("Please Reboot Your PC For Changes To Take Effect!!")
			os.Exit(0)
		case "n", "no":
			log.Printf("Please move the file (%v) into your udev rule directory and reboot for it to take effect, or if you REALLLLY want to run this program as sudo append --sudo to your run command", udevfilename)
			os.Exit(0)
		default:
			log.Fatal("Invalid Answer")
		}
	}
	if err != nil {
		if err == gousb.ErrorAccess {
			log.Print("It looks like the cameras cannot be accessed, udev file being created in this directory")
			log.Printf("Creating UDEV Rule At %v", udevfilename)
			err := os.WriteFile(udevfilename, []byte(udevrule), 0644)
			if err != nil {
				log.Fatalf("Could not Create File: %v", err)
			}
			log.Print("File Created ! Please copy to your udev directory, chown to root, and reboot for it to take effect")
			os.Exit(0)

		} else {
			log.Fatal(err)
		}
	}
	if dev == nil {
		log.Fatal("Could Not Find Device, Please Make Sure It Is On And Plugged In !")
	}
	defer dev.Close()
	return fmt.Sprintf("/dev/bus/usb/%03v/%03v", dev.Desc.Bus, dev.Desc.Address)
}

var gitVersion string
var verbosePtr = flag.Bool("verbose", false, "Whether or not to show libusb errors")
var port = flag.Int("port", 8080, "What Port To Output Frames To")
var version = flag.Bool("version", false, "Flag To Show Current Version")
var sudo = flag.Bool("sudo", false, "Force Program To Run As Sudo")

// mjpegBroadcaster is a correct MJPEG-over-HTTP handler.
// It sends each frame as a properly delimited multipart/x-mixed-replace part
// with an accurate Content-Length header so clients never overread.
type mjpegBroadcaster struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func newMJPEGBroadcaster() *mjpegBroadcaster {
	return &mjpegBroadcaster{clients: make(map[chan []byte]struct{})}
}

func (b *mjpegBroadcaster) update(frame []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- frame:
		default: // drop frame rather than block if client is slow
		}
	}
}

func (b *mjpegBroadcaster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ch := make(chan []byte, 2)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.clients, ch)
		b.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	f, canFlush := w.(http.Flusher)
	for {
		select {
		case frame := <-ch:
			fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(frame))
			w.Write(frame)
			fmt.Fprint(w, "\r\n")
			if canFlush {
				f.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

func main() {
	flag.Parse()
	if *version {
		log.Print("go-bsb-cams " + gitVersion)
		os.Exit(0)
	}
	stream := newMJPEGBroadcaster()
	device := getdevice()
	go imagestreamer(stream, device)
	mux := http.NewServeMux()
	mux.Handle("/stream", stream)
	log.Print("Server Is Running And Can Be Accessed At: http://localhost:" + strconv.Itoa(*port) + "/stream")
	log.Print("Make Sure You Have No Ending / When Inputting The Url Into Baballonia !!!")
	log.Print("If You Are Here And Cannot See The Cams, Please Close This Program, Unplug And Replug Your BSB, And Try Again :)")
	log.Fatal(http.ListenAndServe(":"+strconv.Itoa(*port), mux))
}

func imagestreamer(stream *mjpegBroadcaster, device string) {
	for {
		if err := streamOnce(stream, device); err != nil {
			log.Printf("Stream error: %v — reconnecting in 2s...", err)
		}
		time.Sleep(2 * time.Second)
	}
}

func streamOnce(stream *mjpegBroadcaster, device string) error {
	fd, err := syscall.Open(device, syscall.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open device: %w", err)
	}
	defer syscall.Close(fd)

	ctx, err := uvc.NewUVCDevice(uintptr(fd))
	if err != nil {
		return fmt.Errorf("uvc init: %w", err)
	}

	info, err := ctx.DeviceInfo()
	if err != nil {
		return fmt.Errorf("device info: %w", err)
	}

	for _, iface := range info.StreamingInterfaces {
		for i, desc := range iface.Descriptors {
			fmtDesc, ok := desc.(*descriptors.MJPEGFormatDescriptor)
			if !ok {
				continue
			}
			if i+1 >= len(iface.Descriptors) {
				continue
			}
			frd, ok := iface.Descriptors[i+1].(*descriptors.MJPEGFrameDescriptor)
			if !ok {
				continue
			}

			resp, err := iface.ClaimFrameReader(fmtDesc.Index(), frd.Index())
			if err != nil {
				return fmt.Errorf("claim frame reader: %w", err)
			}

			for {
				fr, err := resp.ReadFrame()
				if err != nil {
					if *verbosePtr {
						log.Printf("Frame read error: %v", err)
					}
					return fmt.Errorf("read frame: %w", err)
				}

				raw, readErr := io.ReadAll(fr)
				if readErr != nil || len(raw) < 4 {
					continue
				}
				// UVC cameras pad frames to USB packet boundaries; trim at JPEG EOI
				if eoi := bytes.LastIndex(raw, []byte{0xFF, 0xD9}); eoi >= 0 {
					raw = raw[:eoi+2]
				}
				if raw[0] != 0xFF || raw[1] != 0xD8 {
					continue
				}
				stream.update(raw)
			}
		}
	}
	return fmt.Errorf("no MJPEG stream found on device")
}
