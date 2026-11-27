import torch, time, statistics
dev = torch.device('cuda')
torch.backends.cuda.matmul.allow_tf32 = True

def timed(fn, warmup=10, iters=50):
    for _ in range(warmup): fn()
    torch.cuda.synchronize()
    ts=[]
    for _ in range(iters):
        s=torch.cuda.Event(True); e=torch.cuda.Event(True)
        s.record(); fn(); e.record(); torch.cuda.synchronize()
        ts.append(s.elapsed_time(e))  # ms
    return statistics.mean(ts), statistics.pstdev(ts)

# 1) GEMM throughput (fp16), real TFLOPS
N=4096
a=torch.randn(N,N,device=dev,dtype=torch.float16); b=torch.randn(N,N,device=dev,dtype=torch.float16)
m,sd=timed(lambda: torch.matmul(a,b))
flops=2*N**3
print(f"GEMM fp16 {N}x{N}: {m:.3f}ms +/- {sd:.3f}  => {flops/(m/1e3)/1e12:.2f} TFLOP/s")

# 2) Real inference: transformer encoder forward (batch of sequences)
import torch.nn as nn
model=nn.TransformerEncoder(nn.TransformerEncoderLayer(d_model=512,nhead=8,dim_feedforward=2048,batch_first=True),num_layers=6).to(dev).half().eval()
x=torch.randn(32,128,512,device=dev,dtype=torch.float16)  # batch 32, seq 128
with torch.no_grad():
    m,sd=timed(lambda: model(x))
print(f"Transformer(6L,d512,b32,seq128) fwd: {m:.3f}ms +/- {sd:.3f}  => {32/(m/1e3):.1f} inferences/s")

# 3) ResNet-style conv inference
conv=nn.Sequential(nn.Conv2d(3,64,7,2,3),nn.ReLU(),nn.Conv2d(64,128,3,1,1),nn.ReLU(),nn.AdaptiveAvgPool2d(1),nn.Flatten(),nn.Linear(128,1000)).to(dev).half().eval()
img=torch.randn(64,3,224,224,device=dev,dtype=torch.float16)
with torch.no_grad():
    m,sd=timed(lambda: conv(img))
print(f"ConvNet(b64,224x224) fwd: {m:.3f}ms +/- {sd:.3f}  => {64/(m/1e3):.1f} images/s")
print("hw: RTX 3060 12GB, torch 2.3.0 cuda12.1, fp16, 50 iters after 10 warmup, CUDA-event timed")
