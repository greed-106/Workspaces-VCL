"""多卡 NCCL 集合通信验证脚本

用途:确认在这台无 NVLink / 无 PCIe P2P 的 10 卡机器上,
容器内的多卡通信(NCCL all-reduce)功能正常。

用 torchrun 启动:
    torchrun --nproc_per_node=<N> --standalone /scripts/nccl-test.py
"""

import os
import time

import torch
import torch.distributed as dist


def main() -> None:
    rank = int(os.environ["RANK"])
    world = int(os.environ["WORLD_SIZE"])
    local_rank = int(os.environ["LOCAL_RANK"])

    if rank == 0:
        print(f"torch {torch.__version__} | cuda {torch.version.cuda} | "
              f"nccl {'.'.join(map(str, torch.cuda.nccl.version()))} | "
              f"可见 GPU {torch.cuda.device_count()} 张 | world_size={world}", flush=True)

    torch.cuda.set_device(local_rank)
    dist.init_process_group(backend="nccl")

    for n_mib in (16, 128):
        n_elem = n_mib * 1024 * 1024 // 4  # fp32

        # 预热(含 NCCL 通信子建立)。注意:all-reduce 会累加,所以预热后必须重置张量,
        # 否则校验值会变成 world_size^(预热次数+计时次数)。
        tensor = torch.ones(n_elem, dtype=torch.float32, device=f"cuda:{local_rank}")
        for _ in range(3):
            dist.all_reduce(tensor)
        torch.cuda.synchronize()

        tensor.fill_(1.0)          # 重置,否则 all-reduce 会一直累加
        torch.cuda.synchronize()

        # 正确性校验:全 1 张量做**一次** all-reduce,结果应恰好等于 world_size
        dist.all_reduce(tensor)
        torch.cuda.synchronize()
        got = tensor[0].item()
        ok = abs(got - world) < 1e-3

        iters = 10
        start = time.time()
        for _ in range(iters):
            dist.all_reduce(tensor)
        torch.cuda.synchronize()
        elapsed = (time.time() - start) / iters

        # 累加一致性:再跑 iters 次后应为 world_size^(1+iters),用相对误差看(仅作参考)
        accum = tensor[0].item()
        expected = float(world) ** (1 + iters)
        accum_rel = abs(accum - expected) / expected

        nbytes = n_elem * 4
        # NCCL 惯例的 bus bandwidth:算法带宽 × 2(world-1)/world
        busbw = nbytes * 2 * (world - 1) / world / elapsed / 1e9

        if rank == 0:
            print(f"[结果] world={world:2d} all_reduce({n_mib:3d}MiB) 单次 {elapsed * 1000:8.2f} ms | "
                  f"busbw {busbw:6.2f} GB/s | 单次校验 {'通过' if ok else '失败'} "
                  f"(期望 {world}, 实际 {got:.0f}) | 累加一致性相对误差 {accum_rel:.2e}", flush=True)
        else:
            assert ok, f"rank {rank} 单次 all-reduce 校验失败: 期望 {world}, 实际 {got}"

    dist.destroy_process_group()


if __name__ == "__main__":
    main()
