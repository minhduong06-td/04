# HUSTack CTF Challenge Design  
## Trusted Runtime — Blackbox Sandbox Escape via Reverse Engineering

> **Mục tiêu tài liệu:** mô tả đầy đủ kiến trúc, intended solution, triển khai web nộp mã C, cơ chế sandbox, privileged broker, hardening và các biện pháp tránh unintended vulnerability/DDoS.  
> **Độ khó dự kiến:** Medium–Hard, khoảng 7.5–8/10.  
> **Loại bài:** Web interface + Linux sandbox reconnaissance + Reverse Engineering + Custom protocol exploitation.

---

## 1. Tóm tắt ý tưởng

Người chơi được cung cấp một website rất đơn giản:

- Nhập mã nguồn C vào textarea; hoặc
- Upload một file `solution.c`;
- Nhấn **Submit**;
- Hệ thống biên dịch và chạy chương trình;
- Trả về `stdout`, `stderr`, trạng thái compile/run và mã thoát.

File chứa flag **không nằm trong filesystem của submission container**. Nó chỉ tồn tại trong một service riêng gọi là `judged broker`.

Mỗi chương trình được launcher nạp kèm một runtime nội bộ:

```text
/opt/hustack/runtime/libhsruntime.so
```

và thừa hưởng một Unix socket đã kết nối ở file descriptor `3`.

Người chơi phải:

```text
Nộp source C hợp lệ
→ reconnaissance bằng /proc
→ phát hiện runtime ELF và fd 3
→ dump libhsruntime.so qua stdout
→ reverse thư viện
→ hiểu handshake, packet và bytecode VM
→ tìm lỗi verifier dùng 16-bit nhưng executor dùng 32-bit
→ tạo bytecode vượt kiểm tra quyền
→ broker đọc private answer object
→ lấy flag
```

Challenge **không sử dụng**:

- Upload ELF giả;
- Thay magic byte;
- Path traversal;
- SQL injection;
- Command injection;
- Kernel exploit;
- Docker escape thật;
- Race condition phụ thuộc timing.

---

# 2. Mục tiêu thiết kế

## 2.1. Trải nghiệm người chơi

Challenge cần tạo cảm giác:

1. Website chỉ là một trình chấm C đơn giản.
2. Sandbox nhìn ban đầu gần như không có gì đáng chú ý.
3. Người chơi tự khám phá `/proc/self/maps` và `/proc/self/fd`.
4. Họ lấy được một file ELF nội bộ.
5. Reverse file đó để tìm giao thức bí mật.
6. Phát hiện lỗi logic giữa verifier và executor.
7. Dùng lỗi đó để vượt ranh giới quyền hạn và đọc answer object.

## 2.2. Ranh giới an toàn

“Sandbox escape” trong challenge chỉ là:

```text
submission không đặc quyền
→ lợi dụng broker để đọc đúng private object của challenge
```

Nó **không phải**:

```text
submission
→ root host
→ Docker daemon
→ filesystem máy chủ
```

Broker chỉ cung cấp một API object cố định. Người chơi không được truyền pathname, command hoặc shellcode cho broker.

---

# 3. Mô tả đề dành cho người chơi

Đề chính nên ngắn, tránh làm lộ hướng giải:

```text
The official answer is stored outside the submission sandbox.

The judge runtime, however, still needs to communicate with it.

Submit a GNU C17 program and recover the flag.
```

Thông tin công khai:

```text
Language: GNU C17
Maximum source size: 10 MB
Time limit: 3 seconds
Memory limit: 128 MB
Output limit: 64 KB
Network access: disabled
```

Không công khai:

- Tên `libhsruntime.so`;
- File descriptor `3`;
- Unix socket;
- Bytecode VM;
- Object ID;
- Cấu trúc packet;
- Lỗi integer-width mismatch;
- Đường dẫn file flag.

---

# 4. Kiến trúc tổng thể

```text
                         ┌────────────────────┐
                         │ Reverse Proxy/WAF  │
                         │ rate limit + TLS   │
                         └─────────┬──────────┘
                                   │
                         ┌─────────▼──────────┐
                         │ Web/API Service    │
                         │ auth + submission  │
                         └─────────┬──────────┘
                                   │ enqueue
                         ┌─────────▼──────────┐
                         │ Job Queue          │
                         │ bounded queue      │
                         └─────────┬──────────┘
                                   │
                         ┌─────────▼──────────┐
                         │ Compiler Worker    │
                         │ GCC, no shell      │
                         └─────────┬──────────┘
                                   │ ELF
                         ┌─────────▼──────────┐
                         │ Submission Sandbox │
                         │ uid 1001           │
                         │ fd 3 connected     │
                         │ runtime injected   │
                         └─────────┬──────────┘
                                   │ Unix socket
                         ┌─────────▼──────────┐
                         │ judged Broker      │
                         │ separate container │
                         │ private answer     │
                         └────────────────────┘
```

## 4.1. Các service khuyến nghị

| Service | Chức năng |
|---|---|
| `reverse-proxy` | TLS, rate limit, body limit, connection limit |
| `web-api` | Nhận source, tạo submission, đọc kết quả |
| `queue` | Redis/RabbitMQ hoặc queue nội bộ có giới hạn |
| `compiler-worker` | Biên dịch source C thành ELF |
| `runner` | Tạo sandbox tạm thời và chạy ELF |
| `judged` | Broker đặc quyền chứa answer object |
| `database` | Lưu metadata submission, không lưu file lớn trực tiếp |
| `object-storage` | Tùy chọn lưu source/result ngắn hạn |

---

# 5. Luồng xử lý submission

```text
POST /api/submissions
        │
        ├─ kiểm tra auth/session
        ├─ kiểm tra rate limit
        ├─ kiểm tra Content-Length
        ├─ kiểm tra đúng một trong hai:
        │      source_text hoặc source_file
        ├─ kiểm tra dung lượng
        ├─ chuẩn hóa newline
        ├─ tạo submission_id ngẫu nhiên
        ├─ lưu source bằng tên nội bộ
        └─ enqueue compile job
```

Worker:

```text
load source
→ tạo workspace riêng
→ gọi gcc bằng argv, không qua shell
→ giới hạn thời gian compile
→ kiểm tra output ELF
→ tạo socketpair
→ khởi động judged session
→ inject runtime
→ chạy chương trình trong sandbox
→ thu stdout/stderr có giới hạn
→ hủy sandbox
→ ghi kết quả
```

---

# 6. Chính sách upload source C

## 6.1. Hard limit 10 MB

Theo yêu cầu, server phải từ chối file lớn hơn:

```text
10 MiB = 10 × 1024 × 1024 = 10,485,760 bytes
```

Kiểm tra ở **ít nhất ba lớp**:

1. Reverse proxy;
2. Web/API;
3. Worker trước khi compile.

Không chỉ tin vào `Content-Length`, vì request chunked có thể không cung cấp giá trị đúng.

### Khuyến nghị thực tế

Source C thông thường rất nhỏ:

- Bài CTF đơn giản: dưới 20 KB;
- Source khai thác dài: thường dưới 100 KB;
- Source hơn 1 MB đã khá bất thường.

Vì vậy:

```text
Hard cap bắt buộc: 10 MiB
Soft warning: 1 MiB
Khuyến nghị production: cân nhắc hard cap 1–2 MiB
```

Nếu muốn giữ đúng thông báo “tối đa 10 MB”, vẫn nên:

- Từ chối trên 10 MiB;
- Ghi log khi source trên 1 MiB;
- Áp dụng compile quota thấp hơn cho source lớn.

## 6.2. Chỉ cho phép một file `.c`

Không nhận:

- ZIP;
- TAR;
- GZIP;
- Object file;
- ELF;
- Thư mục project;
- Multiple files;
- URL tải source;
- Git repository.

Tên file do người dùng gửi **không được dùng trực tiếp trên filesystem**.

Ví dụ:

```text
Tên người dùng: ../../tmp/a.c
Tên nội bộ:     9f12e2b0-4f31-4d11-9b02-8d21c96e8d3a.c
```

Tên gốc chỉ lưu làm metadata sau khi escape ký tự hiển thị.

## 6.3. Extension và nội dung

Extension chỉ là lớp UX, không phải biện pháp bảo mật duy nhất.

API chấp nhận:

```text
filename kết thúc bằng .c
Content-Type:
- text/plain
- text/x-c
- application/octet-stream
```

Không nên tin hoàn toàn MIME từ client.

Có thể từ chối file chứa NUL byte:

```text
0x00
```

vì source C text bình thường không cần NUL nhúng trong file.

Không cần ép UTF-8 tuyệt đối vì source C có thể chứa byte ASCII mở rộng, nhưng nên:

- Cho phép UTF-8;
- Cho phép ASCII;
- Không thực hiện Unicode normalization làm thay đổi byte source;
- Không tự động bỏ magic byte hoặc BOM theo logic tùy tiện;
- Nếu hỗ trợ UTF-8 BOM, chỉ bỏ đúng chuỗi `EF BB BF`.

## 6.4. Không parse source bằng regex bảo mật

Không cố chặn từ khóa như:

```text
open
socket
execve
/proc
```

Đây là challenge sandbox nên các syscall reconnaissance là intended.

Bảo mật phải dựa vào:

- Namespace;
- Seccomp;
- Cgroup;
- Mount policy;
- Broker allowlist;

không dựa vào blacklist source code.

---

# 7. API đề xuất

## 7.1. Tạo submission

```http
POST /api/submissions
Content-Type: multipart/form-data
```

Một trong hai field:

```text
source_text
source_file
```

Không cho gửi đồng thời cả hai.

Response:

```json
{
  "submission_id": "01JZ...",
  "status": "queued"
}
```

## 7.2. Lấy kết quả

```http
GET /api/submissions/{submission_id}
```

Response:

```json
{
  "status": "finished",
  "compile": {
    "success": true,
    "stderr": ""
  },
  "run": {
    "exit_code": 0,
    "signal": null,
    "stdout": "...",
    "stderr": "",
    "time_ms": 41,
    "memory_kb": 9340,
    "truncated": false
  }
}
```

## 7.3. Không dùng endpoint đồng bộ lâu

Không để request upload giữ kết nối cho đến khi compile/run xong.

Sai:

```text
POST /submit → chờ 3–20 giây → trả kết quả
```

Đúng:

```text
POST /submit → trả submission_id ngay
GET /submission/id → polling có rate limit
```

Điều này giảm nguy cơ giữ connection để DDoS.

---

# 8. Rate limiting và chống DDoS

## 8.1. Rate limit theo nhiều chiều

Không chỉ rate limit theo IP vì:

- Một trường học có thể dùng chung NAT;
- Người chơi có thể đổi IP;
- Một tài khoản có thể spam qua nhiều IP.

Nên áp dụng đồng thời:

```text
Per IP
Per account
Per session
Global queue
Per running worker
```

### Giá trị khởi đầu đề xuất

| Hành động | Giới hạn |
|---|---:|
| Tạo submission/account | 6/phút |
| Tạo submission/IP | 20/phút |
| Tạo submission/account | 120/giờ |
| Poll kết quả/account | 60/phút |
| Submission đang chạy/account | 1 |
| Submission đang chờ/account | 3 |
| Queue toàn hệ thống | 500 |
| Source tối đa | 10 MiB |
| Compile timeout | 8 giây |
| Runtime timeout | 3 giây |
| Output | 64 KiB mỗi stream hoặc tổng 64 KiB |

Trong cuộc thi đông người, có thể tăng:

```text
submission/account: 10/phút
```

nhưng vẫn giữ:

```text
concurrent running/account: 1
```

## 8.2. Token bucket

Sử dụng token bucket thay vì chỉ fixed window.

Ví dụ:

```text
Account bucket:
capacity = 6
refill = 1 token / 10 giây
cost mỗi submission = 1
```

Source lớn có thể tốn nhiều token hơn:

```text
<= 256 KiB   cost 1
<= 1 MiB     cost 2
> 1 MiB      cost 3
```

## 8.3. Queue có giới hạn

Không dùng queue vô hạn.

Khi queue đầy:

```http
HTTP 503 Service Unavailable
Retry-After: 15
```

Không tiếp tục lưu hàng nghìn source vào RAM hoặc database.

## 8.4. Circuit breaker

Tự động tạm dừng nhận submission khi:

- CPU worker trên 90%;
- Queue vượt ngưỡng;
- Database latency tăng;
- Broker error rate bất thường;
- Disk tạm gần đầy.

Thông báo:

```text
Judge is temporarily busy. Please retry later.
```

## 8.5. Chống Slowloris

Reverse proxy cần:

- Header timeout ngắn;
- Body timeout;
- Giới hạn keep-alive;
- Giới hạn concurrent connection/IP;
- Không buffer request vô hạn vào RAM.

## 8.6. Ví dụ Nginx

```nginx
http {
    limit_req_zone $binary_remote_addr zone=submit_ip:20m rate=20r/m;
    limit_conn_zone $binary_remote_addr zone=conn_ip:20m;

    client_max_body_size 10m;
    client_body_timeout 10s;
    client_header_timeout 10s;
    send_timeout 15s;
    keepalive_timeout 15s;
    keepalive_requests 100;

    server {
        listen 443 ssl http2;

        location = /api/submissions {
            limit_req zone=submit_ip burst=5 nodelay;
            limit_conn conn_ip 10;

            proxy_request_buffering on;
            proxy_pass http://web_api;
        }

        location /api/submissions/ {
            limit_req zone=submit_ip burst=30;
            proxy_pass http://web_api;
        }
    }
}
```

Lưu ý: rate limit ở Nginx chỉ là lớp ngoài. Web API vẫn phải kiểm tra theo account.

---

# 9. Biên dịch an toàn

## 9.1. Không gọi shell

Không dùng:

```c
system("gcc " + filename);
```

Không dùng:

```python
subprocess.run(command, shell=True)
```

Dùng argv cố định:

```python
subprocess.run(
    [
        "/usr/bin/gcc",
        "-std=gnu17",
        "-O0",
        "-fno-diagnostics-color",
        "-o",
        output_path,
        source_path,
    ],
    shell=False,
    timeout=8,
    cwd=workspace,
    env=clean_environment,
)
```

## 9.2. Compiler flags

Khuyến nghị:

```text
-std=gnu17
-O0
-fno-diagnostics-color
-fno-ident
-Wl,--build-id=none
```

Không cần bật các sanitizer vì có thể:

- Tăng runtime;
- Làm leak thông tin không cần thiết;
- Thay đổi hành vi challenge.

Có thể dùng:

```text
-pipe
```

nhưng cần bảo đảm không tăng memory quá mức.

## 9.3. Include path

Chỉ dùng include path hệ thống tối thiểu.

Không thêm directory do người dùng kiểm soát vào:

```text
LIBRARY_PATH
LD_LIBRARY_PATH
CPATH
C_INCLUDE_PATH
```

Xóa các biến môi trường compiler có thể gây inject:

```text
GCC_EXEC_PREFIX
COMPILER_PATH
LIBRARY_PATH
CPATH
C_INCLUDE_PATH
DEPENDENCIES_OUTPUT
```

## 9.4. Workspace riêng

Mỗi submission:

```text
/work/{submission_uuid}/
├── source.c
├── program
└── compile.log
```

Quyền:

```text
owner: compiler-worker
mode: 0700
```

Sau khi chạy xong phải xóa toàn bộ workspace.

Không tái sử dụng workspace giữa người chơi.

---

# 10. Submission sandbox

## 10.1. User và namespace

Chương trình chạy bằng:

```text
uid=1001
gid=1001
```

Không chạy root.

Dùng:

- User namespace;
- PID namespace;
- Mount namespace;
- IPC namespace;
- UTS namespace;
- Network namespace riêng không có interface ra ngoài.

## 10.2. Capability

```text
cap_drop: ALL
```

Không cấp:

```text
CAP_SYS_ADMIN
CAP_SYS_PTRACE
CAP_DAC_READ_SEARCH
CAP_SYS_CHROOT
CAP_NET_ADMIN
```

## 10.3. Filesystem

Root filesystem:

```text
read-only
```

Writable:

```text
/tmp       tmpfs, size 8 MiB, noexec,nosuid,nodev
/work      tmpfs, size 16 MiB, noexec,nosuid,nodev
```

Executable của submission có thể được bind-mount read-only từ runner.

Không mount:

```text
/var/run/docker.sock
/run/containerd/containerd.sock
/host
/root
/home của server
Kubernetes service account token
compiler source
database credential
SSH key
```

## 10.4. Network

```text
network: none
```

Không DNS, không loopback service ngoài broker.

Broker giao tiếp qua một socketpair hoặc Unix socket được truyền trực tiếp, không cần network namespace mở.

## 10.5. Cgroup limits

Khuyến nghị:

```text
CPU time:       3 giây
Wall time:      4 giây
Memory:         128 MiB
Swap:           0
PIDs:           32
Open files:     64
File size:      2 MiB
Core dump:      0
Locked memory:  0
```

Ví dụ rlimit:

```text
RLIMIT_CPU     = 3
RLIMIT_AS      = 128 MiB
RLIMIT_FSIZE   = 2 MiB
RLIMIT_NOFILE  = 64
RLIMIT_NPROC   = 32
RLIMIT_CORE    = 0
```

## 10.6. Output limit

Không chỉ giới hạn sau khi process kết thúc. Runner phải đọc pipe theo streaming và dừng khi vượt ngưỡng.

Khuyến nghị:

```text
stdout + stderr tổng: 64 KiB
```

Khi vượt:

- Ngừng lưu thêm;
- Đánh dấu `truncated=true`;
- Có thể kill process nếu tiếp tục ghi quá nhanh.

Nếu chỉ ngừng đọc pipe mà không kill, process có thể block và giữ worker. Do đó nên:

```text
output vượt 64 KiB
→ đóng pipe
→ gửi SIGKILL
→ status = Output Limit Exceeded
```

## 10.7. Fork bomb

Ngăn bằng:

```text
RLIMIT_NPROC
cgroup pids.max
```

Cần cả hai vì hành vi tùy runtime/container.

## 10.8. Disk exhaustion

- Workspace tmpfs có quota;
- `RLIMIT_FSIZE`;
- Không cho ghi vào rootfs;
- Dọn sandbox sau timeout/crash;
- Background reaper xóa workspace mồ côi.

---

# 11. Runtime nội bộ

## 11.1. File được inject

```text
/opt/hustack/runtime/libhsruntime.so
```

Launcher sử dụng:

```text
LD_PRELOAD=/opt/hustack/runtime/libhsruntime.so
```

Sau khi loader hoàn tất, runtime nên xóa biến:

```c
unsetenv("LD_PRELOAD");
```

Việc này không nhằm giấu tuyệt đối, chỉ tránh hint quá trực tiếp.

Thư viện vẫn xuất hiện trong:

```text
/proc/self/maps
```

Đây là intended clue.

## 11.2. File descriptor nội bộ

Runner tạo:

```text
socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0, pair)
```

Một đầu đưa cho broker/session handler, một đầu đưa vào submission dưới fd `3`.

Khi `execve`, cần bỏ `FD_CLOEXEC` đúng trên fd được truyền.

Các fd không cần thiết khác phải đóng hết.

Submission thấy:

```text
/proc/self/fd/3 -> socket:[...]
```

## 11.3. Runtime constructor

Constructor không được đọc mất server hello trước `main()`.

Luồng khuyến nghị:

1. Runtime constructor xác nhận fd 3 tồn tại;
2. Gửi một frame `REGISTER` rất nhỏ nếu cần;
3. Không gọi `recv`;
4. `main()` bắt đầu;
5. Server hello vẫn nằm trong socket.

Điều này làm exploit ổn định.

---

# 12. Privileged judged broker

## 12.1. Tách container

Broker chạy trong container riêng:

```text
user: judged
network: none
read_only: true
```

Mount duy nhất dữ liệu challenge:

```text
/srv/judged/private/reference.dat:ro
```

Broker không cần chạy root nếu file thuộc user `judged`.

Ví dụ:

```text
judged:judged
reference.dat mode 0400
```

## 12.2. Không nhận pathname

Broker không có API:

```text
OPEN_PATH
READ_FILE
EXEC
LOAD_LIBRARY
```

Chỉ nhận `object_id`.

Object table:

```c
switch (object_id) {
    case 0:
    case 1:
    case 2:
    case 3:
        return read_public_object(object_id);

    case 0x00010007:
        return read_reference_object();

    default:
        return OBJECT_NOT_FOUND;
}
```

Điều này loại bỏ:

- Path traversal;
- Symlink attack;
- Arbitrary file read;
- Null-byte path confusion;
- Encoding confusion.

## 12.3. Một broker session cho mỗi submission

Không để tất cả submission dùng chung một connection hoặc shared state.

Mỗi submission có:

- `session_id` riêng;
- `nonce` riêng;
- sequence riêng;
- quota request riêng;
- timeout riêng.

## 12.4. Broker quota

Mỗi session:

```text
Tối đa 32 frame
Tối đa 16 EVAL request
Tối đa 24 byte output/request
Tối đa 1 KiB tổng response
Thời gian sống: 10 giây
```

Nếu vượt:

```text
SESSION_QUOTA_EXCEEDED
```

Điều này ngăn submission dùng broker để DDoS nội bộ.

---

# 13. Giao thức nội bộ

## 13.1. Server hello

```c
#pragma pack(push, 1)

struct hs_hello {
    char     magic[4];       // "HSH2"
    uint32_t session_id;
    uint64_t nonce;
    uint32_t runtime_abi;
};

#pragma pack(pop)
```

Kích thước:

```text
20 byte
```

Giá trị:

```text
magic       = HSH2
runtime_abi = 2
```

## 13.2. Request frame

```c
#pragma pack(push, 1)

struct hs_frame {
    char     magic[4];       // "HSJ2"
    uint8_t  version;        // 2
    uint8_t  opcode;
    uint16_t body_length;
    uint32_t session_id;
    uint32_t sequence;
    uint64_t authenticator;
};

#pragma pack(pop)
```

Kích thước header:

```text
24 byte
```

Opcode public:

```text
0x01 PING
0x02 REPORT_USAGE
0x03 REPORT_EXIT
```

Opcode legacy:

```text
0x31 EVAL_PROGRAM
```

## 13.3. Session key

```c
static uint64_t rol64(uint64_t value, unsigned int bits)
{
    return (value << bits) | (value >> (64U - bits));
}

static uint64_t derive_key(
    uint64_t nonce,
    uint32_t session_id
) {
    uint64_t x = nonce ^ 0x91D4B728C63A5F0DULL;

    x += ((uint64_t)session_id << 32) | session_id;
    x ^= x >> 27;
    x *= 0x9E3779B185EBCA87ULL;
    x = rol64(x, 19);
    x ^= 0x4855535441434B32ULL;

    return x;
}
```

Đây không phải crypto bảo mật thật. Nó chỉ buộc người chơi reverse logic.

## 13.4. Authenticator

```c
static uint64_t calculate_authenticator(
    uint64_t session_key,
    uint32_t sequence,
    const uint8_t *body,
    size_t body_length
) {
    uint64_t state =
        session_key ^
        ((uint64_t)sequence << 32) ^
        body_length;

    for (size_t i = 0; i < body_length; i++) {
        state ^= body[i];
        state *= 0x100000001B3ULL;
        state = rol64(state, 9);
        state ^= state >> 23;
    }

    return state ^ 0x4A55444745525432ULL;
}
```

Broker phải kiểm tra:

```text
magic
version
body_length
session_id
sequence
authenticator
session quota
```

Sequence phải tăng chính xác để ngăn replay trong cùng session.

---

# 14. Bytecode VM

## 14.1. EVAL body

```c
#pragma pack(push, 1)

struct eval_request {
    uint8_t  vm_version;
    uint8_t  instruction_count;
    uint16_t code_length;
    uint32_t requested_output_size;
    uint8_t  code[];
};

#pragma pack(pop)
```

Giới hạn:

```text
vm_version             = 1
instruction_count      <= 32
code_length            <= 96
requested_output_size  <= 24
```

## 14.2. Instruction set

| Opcode | Tên | Operand |
|---:|---|---|
| `0x01` | `PUSH8` | `imm8` |
| `0x02` | `PUSH16` | `imm16` little-endian |
| `0x03` | `PUSH32` | `imm32` little-endian |
| `0x10` | `ADD` | none |
| `0x11` | `XOR` | none |
| `0x12` | `OR` | none |
| `0x13` | `SHL` | none |
| `0x20` | `SET_OFFSET` | none |
| `0x21` | `SET_LENGTH` | none |
| `0x30` | `READ_OBJECT` | none |
| `0xFF` | `HALT` | none |

Stack depth tối đa:

```text
16
```

Verifier cần kiểm tra:

- Opcode hợp lệ;
- Operand không vượt code buffer;
- Stack underflow;
- Stack overflow;
- Có `HALT`;
- Instruction count đúng;
- Offset/length hợp lệ;
- Object ID public.

---

# 15. Intended vulnerability

## 15.1. Verifier dùng 16-bit

```c
struct verifier_vm {
    uint16_t stack[16];
    int sp;
};
```

Ví dụ:

```c
uint16_t value = pop16(vm);
uint16_t bits  = pop16(vm);
push16(vm, value << bits);
```

Khi `READ_OBJECT`:

```c
uint16_t object_id = pop16(vm);

if (object_id >= 16) {
    return VERIFY_DENIED;
}
```

## 15.2. Executor dùng 32-bit

```c
struct execution_vm {
    uint32_t stack[16];
    int sp;
};
```

```c
uint32_t value = pop32(vm);
uint32_t bits  = pop32(vm);
push32(vm, value << bits);
```

Khi đọc:

```c
uint32_t object_id = pop32(vm);
return read_object(object_id, offset, length);
```

## 15.3. Private object

```text
0x00010007
```

Bytecode logic:

```text
PUSH32 offset
SET_OFFSET
PUSH8  24
SET_LENGTH

PUSH8  1
PUSH8  16
SHL
PUSH8  7
OR

READ_OBJECT
HALT
```

Verifier:

```text
uint16:
1 << 16 = 0
0 | 7   = 7
7 < 16  = allowed
```

Executor:

```text
uint32:
1 << 16 = 0x00010000
0x00010000 | 7 = 0x00010007
```

Broker đọc private reference object.

---

# 16. Response frame

Response nên có cấu trúc cố định:

```c
#pragma pack(push, 1)

struct hs_response {
    char     magic[4];       // "HSR2"
    uint16_t status;
    uint16_t data_length;
    uint32_t sequence;
    uint32_t object_size;
    uint64_t authenticator;
    uint8_t  data[];
};

#pragma pack(pop)
```

Status:

```text
0x0000 OK
0x0001 MALFORMED
0x0002 BAD_SESSION
0x0003 BAD_SEQUENCE
0x0004 BAD_AUTH
0x0010 VM_INVALID
0x0011 VM_DENIED
0x0012 OBJECT_NOT_FOUND
0x0013 READ_LIMIT
0x0020 SESSION_QUOTA
```

Không trả thông báo chi tiết kiểu:

```text
Verifier accepted object 7 but executor read 0x00010007
```

---

# 17. Cách người chơi lấy file nội bộ

Intended reconnaissance:

```text
/proc/self/maps
/proc/self/fd
/proc/self/status
/proc/self/exe
```

Từ `/proc/self/maps`, tìm:

```text
/opt/hustack/runtime/libhsruntime.so
```

Người chơi có thể `open()` file runtime và base64/hex dump qua stdout.

## 17.1. Giới hạn dump

Runtime nên khoảng:

```text
50–90 KiB
```

Output limit challenge:

```text
64 KiB
```

Có thể cần 2 submission để dump toàn bộ. Không nên làm runtime vài MB vì việc ghép file sẽ trở thành thao tác nhàm chán.

## 17.2. Build runtime

Khuyến nghị:

```text
ELF x86_64
shared object
stripped
PIE-compatible
không pack
không anti-debug nặng
```

Có thể:

- Strip symbol;
- Giữ vài chuỗi error code;
- Inline một phần hàm;
- Để self-test function chứa private object ID.

Không nên dùng UPX hoặc control-flow flattening.

---

# 18. Blackbox clues

Blackbox vẫn cần tín hiệu đủ để người chơi tiến triển.

## 18.1. `/proc` được phép đọc

Không hide hoàn toàn `/proc/self/maps`, vì đây là clue chính.

Chỉ mount proc của namespace submission, không phải host proc.

## 18.2. fd 3

`/proc/self/fd/3` cho thấy socket, nhưng không nói broker là gì.

## 18.3. Phản hồi lỗi

Gửi rác vào fd 3:

```text
HSR2 + status MALFORMED
```

Gửi đúng magic sai auth:

```text
status BAD_AUTH
```

Bytecode sai:

```text
status VM_INVALID
```

Các error code đủ phân biệt stage nhưng không làm lộ vulnerability.

---

# 19. Hint đề xuất

## Hint 1

```text
Every submitted program shares part of its address space with the judge runtime.
```

## Hint 2

```text
One inherited descriptor is not used for stdin, stdout, or stderr.
```

## Hint 3

```text
The judge validates a program before executing it. The two stages do not agree on the size of every value.
```

Có thể mở hint theo thời gian:

```text
T+60 phút: Hint 1
T+120 phút: Hint 2
T+180 phút: Hint 3
```

---

# 20. Bảo vệ database và tránh SQL injection

Khả năng challenge cần database để lưu:

- User;
- Session;
- Submission metadata;
- Status;
- Điểm;
- Thời gian.

## 20.1. Chỉ dùng parameterized query

Sai:

```python
sql = "SELECT * FROM submissions WHERE id = '" + submission_id + "'"
```

Đúng:

```python
cursor.execute(
    "SELECT * FROM submissions WHERE id = %s AND user_id = %s",
    (submission_id, user_id),
)
```

Hoặc dùng ORM, nhưng vẫn tránh raw query ghép chuỗi.

## 20.2. ID không tuần tự

Dùng:

- UUIDv4;
- UUIDv7;
- ULID có entropy đủ.

Không dùng:

```text
submission/1
submission/2
submission/3
```

để tránh IDOR dễ đoán.

Mọi truy vấn submission phải có:

```text
WHERE submission_id = ? AND owner_user_id = ?
```

Admin/scoreboard dùng quyền riêng.

## 20.3. Không lưu source vào SQL nếu không cần

Khuyến nghị:

- DB lưu metadata;
- Source lưu object storage hoặc filesystem riêng;
- Tên object là UUID;
- Retention ngắn.

Nếu vẫn lưu source trong DB:

- Dùng BLOB/TEXT parameterized;
- Giới hạn 10 MiB trước khi insert;
- Không log toàn bộ source.

## 20.4. Quyền database tối thiểu

Web API account chỉ được:

```text
SELECT/INSERT/UPDATE trên bảng cần thiết
```

Không cấp:

```text
CREATE USER
DROP DATABASE
SUPERUSER
FILE
```

Worker không cần quyền đọc bảng user/password.

## 20.5. Migration và admin

Không expose migration/admin endpoint trên public network.

---

# 21. Các lỗi web khác cần tránh

## 21.1. Command injection

Không ghép:

- Filename;
- Compiler option;
- User language;
- Submission ID;

vào shell command.

## 21.2. IDOR

Người dùng chỉ xem được submission của mình, trừ khi challenge cố ý public source — không khuyến nghị.

## 21.3. Stored XSS

Source, compiler error, stdout và stderr đều là dữ liệu không tin cậy.

Frontend phải render bằng:

```text
textContent
```

không dùng:

```text
innerHTML
```

Compiler output có thể chứa HTML/JS do source tự in ra.

Escape:

```text
< > & " '
```

## 21.4. CSRF

Nếu dùng cookie session:

- `SameSite=Lax` hoặc `Strict`;
- CSRF token cho POST;
- Kiểm tra Origin/Referer.

Nếu dùng bearer token, không lưu trong localStorage nếu có thể.

## 21.5. CORS

Chỉ cho origin chính thức.

Không:

```text
Access-Control-Allow-Origin: *
Access-Control-Allow-Credentials: true
```

## 21.6. Session cookie

```text
HttpOnly
Secure
SameSite=Lax
```

Rotate session sau login.

## 21.7. SSRF

Không cho người dùng nhập URL để tải source.

Không có:

```text
Import from URL
Import from GitHub
Fetch testcase
```

## 21.8. Archive extraction

Không nhận ZIP/TAR, do đó loại bỏ:

- Zip Slip;
- Decompression bomb;
- Symlink trong archive;
- Parser differential.

## 21.9. Template injection

Không đưa source hoặc output trực tiếp vào server-side template expression.

## 21.10. Log injection

Khi log filename/output:

- JSON structured logging;
- Escape newline/control character;
- Không cho người dùng giả dòng log mới.

---

# 22. Compiler và runner abuse

## 22.1. Compile bomb

Source C có thể khiến compiler dùng nhiều CPU/RAM, ví dụ macro/template-like expansion.

Biện pháp:

```text
Compile wall timeout: 8 giây
Compile memory: 512 MiB
Compiler process count: 16
Compiler output: 64 KiB
Workspace: 32 MiB
```

Compiler worker cũng phải chạy trong container riêng, không chạy trực tiếp trên host.

## 22.2. Linker abuse

Không cho user truyền compiler flag.

Không hỗ trợ pragma/linker input từ file ngoài workspace.

Workspace chỉ chứa đúng source và output.

## 22.3. Runtime infinite loop

Giới hạn cả:

- CPU time;
- Wall time.

Wall time quan trọng vì process có thể sleep hoặc block.

## 22.4. Process không chịu chết

Khi timeout:

1. Gửi `SIGKILL` cho toàn bộ cgroup;
2. Chờ reap;
3. Hủy namespace/container;
4. Xóa workspace.

Không chỉ kill PID chính vì child process có thể còn sống.

---

# 23. Broker hardening để chỉ còn intended vulnerability

## 23.1. Parser bounds-check đầy đủ

Mặc dù verifier có lỗi logic 16/32-bit cố ý, mọi lỗi memory safety khác phải được tránh:

- Kiểm tra `body_length`;
- Kiểm tra code length;
- Kiểm tra operand;
- Kiểm tra stack;
- Không integer overflow khi cộng offset;
- Không copy vượt buffer;
- Không format string từ user.

## 23.2. Không dùng C string cho packet

Packet là binary. Dùng length rõ ràng, không dùng:

```c
strlen(packet)
strcpy
sprintf
```

Dùng:

```c
memcpy
snprintf
read_exact
```

## 23.3. Object read an toàn

```c
if (offset > object_size) reject;
if (length > 24) reject;
if (offset + length < offset) reject;
if (offset + length > object_size) clamp_or_reject;
```

Nên reject thay vì clamp để giao thức rõ ràng.

## 23.4. Broker không chạy shell

Không gọi command ngoài.

Private object được mở một lần lúc startup bằng đường dẫn hardcode trong config nội bộ.

## 23.5. Không expose broker socket public

Socket chỉ tồn tại trong runtime/session orchestration.

Không bind TCP port.

---

# 24. Flag management

## 24.1. Flag không hardcode trong image public

Trong production:

- Inject flag qua secret mount read-only;
- Hoặc generate flag per deployment;
- Broker đọc lúc startup.

Không để flag trong:

- Git repository;
- Docker build layer public;
- Compiler image;
- Runtime library;
- Web frontend;
- Database migration.

## 24.2. Per-team flag

Nếu nền tảng hỗ trợ dynamic flag:

```text
private object = HMAC(master_secret, team_id || challenge_id)
```

Broker cần biết `team_id` của session từ runner qua kênh tin cậy, không lấy từ packet submission.

## 24.3. Không log flag

Broker log:

```text
object_id, offset, length, status
```

Không log response data.

Web/runner cũng không ghi stdout đầy đủ vào log hệ thống; chỉ lưu vào result có access control.

---

# 25. Logging và giám sát

## 25.1. Metrics

Theo dõi:

```text
submission_created_total
submission_rejected_total
compile_timeout_total
runtime_timeout_total
output_limit_total
queue_depth
active_workers
broker_bad_auth_total
broker_vm_invalid_total
broker_session_quota_total
upload_bytes
```

## 25.2. Alert

Cảnh báo khi:

- Queue tăng liên tục;
- Một account có tỷ lệ timeout cao;
- Nhiều source gần 10 MiB;
- Nhiều request `BAD_AUTH`;
- Worker restart liên tục;
- Disk tmp trên 80%;
- Submission latency p95 tăng mạnh.

## 25.3. Privacy

Không log toàn bộ source trừ khi phục vụ debug nội bộ và có retention ngắn.

Không hiển thị IP người chơi công khai.

---

# 26. Retention và cleanup

Khuyến nghị:

```text
Workspace: xóa ngay sau job
Compiled ELF: xóa ngay sau job
Source: giữ 24–72 giờ
stdout/stderr: giữ đến hết contest + thời gian audit
Broker session: xóa ngay sau submission
```

Có cron/reaper:

```text
xóa workspace cũ hơn 15 phút
xóa container mồ côi
xóa queue job stale
```

---

# 27. Kiểm thử challenge

## 27.1. Functional tests

- Paste C hợp lệ;
- Upload `.c` hợp lệ;
- Compile error;
- Runtime error;
- Timeout;
- Memory limit;
- Output limit;
- File 10 MiB;
- File 10 MiB + 1 byte;
- NUL byte;
- Filename Unicode;
- Filename traversal;
- Concurrent submission.

## 27.2. Security tests

- SQL injection trong submission ID;
- XSS trong stdout;
- XSS trong compiler error;
- CSRF;
- IDOR;
- Oversized multipart;
- Chunked upload vượt limit;
- Slow upload;
- Fork bomb;
- Disk fill;
- Infinite output;
- Compile bomb;
- Child process sống sau timeout;
- Attempt access Docker socket;
- Attempt access host proc;
- Attempt outbound network;
- Attempt broker packet quá dài.

## 27.3. Intended solve test

Một solver nội bộ phải chứng minh:

1. Tìm runtime qua `/proc/self/maps`;
2. Tìm fd 3;
3. Dump runtime hoàn chỉnh;
4. Reverse đủ protocol;
5. Derive session key;
6. Tạo authenticator;
7. Tạo VM program;
8. Lấy private answer;
9. Đọc flag theo nhiều offset nếu cần.

## 27.4. Unintended solution audit

Kiểm tra:

- Flag có vô tình nằm trong image runner không;
- `strings` trên web/compiler image có flag không;
- Environment có secret không;
- `/proc/self/environ` có credential không;
- Runtime có private object data không;
- Broker error có leak flag không;
- Core dump có leak không;
- Logs có leak không;
- Shared volume có source/flag của submission khác không.

---

# 28. Cân bằng độ khó

## 28.1. Mức Medium–Hard

- `/proc/self/maps` đọc được;
- Runtime file readable;
- Runtime stripped nhưng không obfuscate;
- Giao thức có magic rõ;
- Session key custom đơn giản;
- Object ID xuất hiện trong self-test dưới dạng phép toán;
- VM chỉ khoảng 10 opcode;
- Mỗi response trả 24 byte.

## 28.2. Tăng lên Hard thấp

- Runtime 80–100 KiB;
- Một số hàm inline;
- Response có authenticator riêng;
- Cần 2–3 request offset;
- Private object ID dựng từ nhiều phép toán;
- Sequence/state machine chặt hơn.

## 28.3. Không nên thêm

- Kernel CVE;
- Heap exploit;
- ROP;
- Race condition;
- Anti-debug mạnh;
- UPX;
- Custom crypto nặng;
- Proof-of-work quá lâu;
- Random behavior.

Những yếu tố này làm challenge thiếu ổn định hoặc lệch khỏi ý tưởng chính.

---

# 29. Giao diện web tối giản

Trang chính:

```text
Challenge title
Description
Language: GNU C17

[ textarea source ]

hoặc

[ Choose solution.c ]

[ Submit ]
```

Trang kết quả:

```text
Status: Finished
Compile: Success
Run: Exit 0
Time: 41 ms
Memory: 9340 KB

stdout:
...

stderr:
...
```

Không cần:

- File manager;
- Multiple language;
- Custom compiler flags;
- Interactive terminal;
- Upload project;
- Testcase editor.

Càng ít chức năng, bề mặt tấn công càng nhỏ.

---

# 30. Trạng thái submission

State machine:

```text
queued
→ compiling
→ compile_failed
→ running
→ finished
```

Trạng thái kết thúc:

```text
compile_error
runtime_error
time_limit
memory_limit
output_limit
internal_error
finished
```

Không hiển thị stack trace backend cho người chơi.

Internal error chỉ trả:

```text
Judge internal error. Reference ID: ...
```

Chi tiết nằm trong log admin.

---

# 31. Docker/OCI hardening mẫu

Ví dụ định hướng, cần điều chỉnh theo hạ tầng:

```yaml
runner:
  read_only: true
  network_mode: none
  cap_drop:
    - ALL
  security_opt:
    - no-new-privileges:true
  pids_limit: 32
  mem_limit: 128m
  memswap_limit: 128m
  tmpfs:
    - /tmp:size=8m,noexec,nosuid,nodev
    - /work:size=16m,noexec,nosuid,nodev
```

Lưu ý:

- `no-new-privileges` phù hợp vì challenge không dùng SUID;
- Không dùng `privileged: true`;
- Không mount Docker socket;
- Không share PID namespace host;
- Không share network host;
- Không mount `/proc` host.

Tốt hơn nữa có thể dùng:

- gVisor;
- Kata Containers;
- Firecracker microVM;
- nsjail/bubblewrap kết hợp cgroup.

Đối với CTF public, Firecracker/gVisor an toàn hơn chạy container thường nếu ngân sách hạ tầng cho phép.

---

# 32. Kiến trúc triển khai khuyến nghị

## 32.1. Phương án vừa phải

```text
Nginx
FastAPI/Go API
PostgreSQL
Redis queue
Worker containers
Docker rootless hoặc gVisor
Broker container riêng
```

## 32.2. Phương án an toàn cao

```text
CDN/WAF
API service
Queue
Ephemeral Firecracker microVM per submission
Broker microservice isolated
Per-team secret object
Central metrics
```

## 32.3. Không chạy compiler/runner trong web process

Web API không được gọi GCC hoặc chạy binary trực tiếp.

Tách worker để:

- Giới hạn tài nguyên;
- Scale riêng;
- Restart khi crash;
- Giảm tác động khi bị khai thác.

---

# 33. Checklist trước khi mở challenge

## Web

- [ ] TLS hoạt động;
- [ ] Rate limit IP và account;
- [ ] CSRF/CORS đúng;
- [ ] Output render bằng text;
- [ ] Không stack trace public;
- [ ] Body hard cap 10 MiB;
- [ ] Multipart streaming limit;
- [ ] Submission ID không đoán được;
- [ ] Authorization theo owner.

## Database

- [ ] Parameterized query;
- [ ] DB user quyền tối thiểu;
- [ ] Không raw SQL ghép chuỗi;
- [ ] Không log source/flag;
- [ ] Backup và retention phù hợp.

## Compiler

- [ ] Không `shell=True`;
- [ ] Clean environment;
- [ ] Compile timeout;
- [ ] Compile memory limit;
- [ ] Workspace riêng;
- [ ] Không user compiler flags.

## Runner

- [ ] UID không root;
- [ ] Capabilities drop all;
- [ ] Network none;
- [ ] Read-only rootfs;
- [ ] Cgroup CPU/RAM/PID;
- [ ] Output streaming limit;
- [ ] Kill cả cgroup;
- [ ] Cleanup sau job;
- [ ] Không Docker socket;
- [ ] Không host mounts.

## Broker

- [ ] Container riêng;
- [ ] Không TCP public;
- [ ] Object allowlist;
- [ ] Không pathname;
- [ ] Quota/session;
- [ ] Bounds-check packet;
- [ ] Không command execution;
- [ ] Không log flag;
- [ ] Chỉ intended 16/32-bit mismatch tồn tại.

## Challenge

- [ ] Runtime dump được;
- [ ] Runtime không quá lớn;
- [ ] Reverse không cần anti-debug;
- [ ] Hint đủ nhưng không lộ;
- [ ] Solver nội bộ giải thành công;
- [ ] Không có unintended easier path.

---

# 34. Intended solution tóm tắt dành cho author

```text
1. Submit C đọc /proc/self/maps.
2. Phát hiện /opt/hustack/runtime/libhsruntime.so.
3. Đọc /proc/self/fd và phát hiện fd 3 là socket.
4. Dump runtime library theo từng đoạn qua stdout.
5. Reverse library.
6. Tìm HSH2 hello và HSJ2 request.
7. Reverse derive_key() và authenticator.
8. Tìm opcode EVAL_PROGRAM.
9. Reverse bytecode verifier/executor.
10. Nhận ra verifier dùng uint16_t, executor dùng uint32_t.
11. Tạo object ID:
       (1 << 16) | 7
12. Verifier thấy 7, executor thấy 0x00010007.
13. Gửi request đọc offset 0, 24, 48...
14. Ghép response và lấy flag.
```

---

# 35. Kết luận

Thiết kế cuối cùng đạt các yêu cầu:

- Web rất đơn giản, chỉ nộp source C;
- Không dựa vào upload magic byte;
- Không dùng path traversal;
- Blackbox nhưng có clue hợp lý;
- Người chơi phải lấy file nội bộ rồi reverse;
- Lỗ hổng là semantic mismatch giữa verifier và executor;
- Có cảm giác vượt quyền nhưng không escape host thật;
- Có rate limit, queue limit và cgroup để chống DDoS;
- Hard cap upload 10 MiB;
- Tránh SQL injection bằng parameterized query;
- Tránh XSS, command injection, SSRF, IDOR và archive attack;
- Broker chỉ đọc object allowlist nên phạm vi ảnh hưởng được kiểm soát;
- Độ khó phù hợp Medium–Hard.

Tên challenge đề xuất:

```text
HUSTack — Trusted Runtime
```

Tên thay thế:

```text
Judge Within
Inherited Trust
Reference Leak
Verifier's Blind Spot
The Third Descriptor
```
