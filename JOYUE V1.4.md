# JOYUE V1.4

**版本**: Final-V1.4
**状态**: Release **摘要**: JOYUE 是一种专为高性能区块链场景设计的 **存算分离** 与 **矩阵式分片** 架构。系统秉持“乐观执行 + 权威纠错”的设计哲学，利用 **MVCC (多版本并发控制)** 替代传统悲观锁机制，结合独创的 **Matrix 2PC (矩阵二阶段提交)** 协议，有效解决了大规模分布式系统中的状态冲突与跨分片网络延迟问题，实现了高并发下的低延迟确认。

---

## 1. 核心设计哲学 

1.  **读写分离网络**:
    * **读**: 依赖 P2P 网络的 Gossip 协议，使用**缓存**。
    * **写**: 依赖 RPC 直连，进行跨分片**权威结算**。
2.  **乐观预测**: 代理端总是假设缓存是准的，先生成意图，不等待网络确认。
3.  **惰性纠错**: 主合约端仅在 Guard 校验失败（过期）时，才启动逻辑树进行重算，否则走快速通道。
4.  **矩阵原子性**: 将批量跨分片交易建模为二维矩阵，通过行列扫描实现高效的二阶段提交。

---

## 2. 实体定义与职责

## 2.1.双合约模型

在 JOYUE 分片系统中，智能合约被解耦为两种物理形态：**主合约 (Master Contract)** 与 **代理合约 (Agent Contract)**。系统采用“用户部署主合约，系统自动分发代理合约”的策略。

- **示例**: 若用户在 Shard A 部署主合约 C，系统将自动在 Shard B 和 Shard C 部署对应的代理合约 C'。
- **原则**: 代理合约仅作为**逻辑执行者**，负责产出意图；主合约作为**最终裁决者**，负责状态变更。

### 2.1.1. 主合约

* **部署**: 用户指定的目标分片 (权威数据所在地)。
* **数据源**: **权威状态数据库 **。对于跨分片依赖，可在 Batch 执行前通过 RPC 预取相关状态。
* **核心职责**：
    * **协调者**: 持有 **逻辑树**。负责矩阵切分、RPC 协调、以及**本地重试纠错**。
    * **参与者**: 持有 **状态数据**。负责 Guard 校验、Delta 应用。

### 2.1.2. 代理合约

* **部署**: 全网除主合约外的所有分片
* **数据源**: **P2P 缓存** (最终一致性，可能过期)。
* **行为约束**: **严禁发起 RPC 请求** (防止 DDoS 主节点)。
* **职责**:
  * 基于缓存执行业务逻辑预演。
  * 生成 **用户意图**，包含 `{Guard, Delta}`。
  * 将意图发送给所属的主合约 。

---

## 3. 分片Leader节点打包策略

### 3.1.代理合约所在分片：基于亲和性的并行打包

在代理合约所在分片，Leader 节点采用 **交易亲和性调度 (Affinity Scheduling)** 策略：

1. **交易分组**: 根据交易 `to` 地址及其依赖关系进行预分组。
2. **核心绑定**: 利用多核 CPU 特性，将特定分组绑定至固定 CPU Core 执行，实现**无锁并行处理**，一个core可以对应多个组，但是一个组只能对应一个core。
3. **批量发送**: 分组执行完成后，生成意图包并路由至对应的主合约分片。一个组生成一个意图包，该意图宝中包含了该组的所有交易的Guard和Delta数据。

### 3.2.主合约所在分片：批量验证与提交

当主合约所在分片的 Leader 收到意图包（状态增量请求）时：

1. **批处理验证**: 调用合约的 `BatchVerify` 接口，对包内所有交易的 `Guard` 进行批量校验。
2. **原子提交**: Guard验证通过后的交易，可以进入到下一步提交阶段，否则丢弃该交易信息并返回交易失败

## 4.逻辑执行树(图)

逻辑执行树是智能合约业务逻辑的 **扁平化、静态化、图表化** 表达。

**核心哲学**: **“惰性纠错”**。只有当代理节点生成的意图在链上验证失败（如版本过期、余额不足）时，主合约才唤醒逻辑树，基于最新状态进行局部重算。

**执行模型**: **部分确定性重放机**。它能够信任意图中已通过的部分，仅从失败点开始引入新数据并可能产生新的执行分支。

### 4.1.数据结构定义

为了支持高效的虚拟机执行与断点映射，逻辑树采用了“数组模拟图”的紧凑结构。

#### 4.1.1.逻辑树

```go
// LogicTree: 对应一个合约函数 (Function Selector) 的完整控制流图
type LogicTree struct {
    Selector [4]byte     // 函数选择器 (Key)
    Version  uint32      // 版本号 (用于合约升级)
    Nodes    []LogicNode // 扁平化的指令节点数组
}

// LogicNode: 单个指令节点
type LogicNode struct {
    OpCode    OpType  // 指令类型 (OP_IF, OP_CALC, OP_EMIT_D...)
    Args      []byte  // 静态参数 (如常量、函数入参索引)
    
    // --- 控制流跳转 (数组索引) ---
    Next      int16   // 顺序执行下一跳 (-1 结束)
    JumpTrue  int16   // 条件为真跳转
    JumpFalse int16   // 条件为假跳转
    
    // --- 意图映射 (核心) ---
    // 标识该节点对应 UnifiedIntent 中的第几个 Guard。
    // -1 表示该节点不产生 Guard (如纯计算或 Delta 生成节点)。
    GuardIndex int16   
}
```

#### 4.1.2.重试上下文 (运行时)

```go
type ReplayContext struct {
    // 1. 瞬时内存 (Transient Memory)
    // 模拟重放过程中的变量状态，包含已执行 Delta 的累积结果
    Memory map[string][]byte

    // 2. 权威数据源
    // - 对于快进节点：读取旧意图 (Old Intent)
    // - 对于重算节点：读取 Master 返回的最新值 (Dirty Values)
    Source DataSource 

    // 3. 执行指针
    PC         int16 // Program Counter
    DeltaIndex int   // 指向旧意图 Delta 列表的消费指针
}
```

### 4.2.专用指令集

逻辑树的指令集比 Guard/Delta 更丰富，因为它需要具备 **“图灵完备性”**（在 Gas 限制内）来模拟业务逻辑。

| **指令集分类** | **OpCode**      | **含义**       | **栈操作 (Stack behavior)** | **典型用途**                  |
| -------------- | --------------- | -------------- | --------------------------- | ----------------------------- |
| **流控制**     | `OP_IF`         | 条件分支       | Pop 1 bool -> Jump          | `if (Stock > 0)`              |
|                | `OP_JUMP`       | 无条件跳转     | None -> Jump                | `while` 循环, `else` 结束跳转 |
|                | `OP_STOP`       | 终止           | None                        | 逻辑结束                      |
| **状态读取**   | `OP_LOAD`       | 读状态         | Push DB[Key]                | 获取当前余额/库存             |
| **参数读取**   | `OP_ARG`        | 读输入         | Push TxArgs[Index]          | 获取用户购买数量              |
| **计算**       | `OP_CALC`       | 算术运算       | Pop A, B -> Push Result     | `Total = Price * Count`       |
| **意图生成**   | **`OP_EMIT_D`** | **产出 Delta** | Pop Key, Val, Op            | **生成修正后的 Delta**        |

### 4.3. 运行机制：快进与重放

逻辑树虚拟机 (VM) 的执行流程分为两个截然不同的阶段，以此实现性能最大化。

#### 4.3.1. 阶段一：确定性快进

**适用范围**: `CurrentNode.GuardIndex < FailureIndex` (失败点之前的所有节点)。

在此阶段，VM **不进行任何逻辑判断**，而是信任代理节点生成的旧意图。

1. **跳过校验**: 直接读取旧 Guard 的结果，决定跳转路径。
2. **Delta 重放 (关键)**:
   - 如果路径上包含 `OP_EMIT_D` (生成 Delta)，VM 从旧意图中取出对应的 Delta。
   - 将 Delta 的值应用到 `ReplayContext.Memory` 中。
   - **目的**: 确保后续节点读取变量时，能看到之前操作产生的副作用（如余额已扣除）。

#### 4.3.2. 阶段二：动态纠错 (Dynamic Branching)

**适用范围**: `CurrentNode.GuardIndex >= FailureIndex` (失败点及之后)。

在此阶段，VM 切换回**标准执行模式**。

1. **注入新值**: 当执行到失败的 Guard 节点时，从 `ReplayContext` 读取 Master 提供的最新权威值。
2. **逻辑重算**: 结合瞬时内存中的值与最新权威值，重新评估条件 (`EvalCondition`)。
3. **路径分叉**:
   - 如果条件结果改变（例如 `True` -> `False`），VM 将跳转到 `JumpFalse` 指向的新分支。
   - 这可能导致后续生成完全不同的一组 Guard 和 Delta。
4. **生成新意图**: 后续所有操作均视为新逻辑，生成的 Guard/Delta 加入 `NewUnifiedIntent`。

### 4.4. 完整工作流图示

**场景**:

1. `Guard_1` (Pass) -> `Delta_1` (Sub 10)
2. `Guard_2` (Fail, NewVal=5) -> ...

| **步骤**   | **节点类型** | **动作**               | **内存状态 (Memory)**           | **输出 (New Intent)** |
| ---------- | ------------ | ---------------------- | ------------------------------- | --------------------- |
| **Step 1** | `Guard_1`    | **快进** (Skip Check)  | Empty                           | Copy `Guard_1`        |
| **Step 2** | `Delta_1`    | **重放** (Apply Delta) | `Balance = Balance - 10`        | Copy `Delta_1`        |
| **Step 3** | `Guard_2`    | **重算** (Re-Eval)     | 读取 Memory，发现余额不足       | Generate `Guard_2'`   |
| **Step 4** | Branch       | **分叉**               | 进入 Else 分支 (如报错或改逻辑) | ...                   |

## 5. 核心机制详解

### 5.1. 智能指令集: Guard + Delta

JOYUE 摒弃了传统的代码传输，改为传输 **意图 (Intent)**，即 `{Guard, Delta}`。

* **Guard (门禁)**: 执行的前置条件，数据类型是一个集合，其中的每一个元素我们可以理解成在合约中的一次条件判断语句。
  
    * **执行策略**:
      
        - **Fast Path (快路径)**: 针对原子数值比较 (如 `Stock >= 1`)，直接进行内存比对，性能极高。
        - **Slow Path (慢路径)**: 针对复杂混合逻辑 (如 `Total + Input < Limit`)，启用基于 **逆波兰表达式 (RPN)** 的轻量级规则引擎。
        
        ```json
        "guards": [
          // 情况 1: 简单的原子 Guard (走快路径，高性能)
          { 
            "key": "Stock", 
            "op": "GTE", 
            "val": "1" 
          },
        
          // 情况 2: 复杂的混合逻辑 (走规则引擎，高灵活性)
          {
            "key": "ComplexRule",
            "op": "EXPR", // <--- 只有看到这个标记，才调用引擎
            "val": [
              { "type": "VAR", "val": "Total" },
              { "type": "ARG", "val": "0" }, //这里是函数的input，表示Total + input < 1000
              { "type": "OP",  "val": "ADD" },
              { "type": "CONST", "val": "1000" },
              { "type": "OP",  "val": "LT" }
            ]
          }
        ]
        ```
    
        我们通过op来判断是否走规则引擎判断，这里我们对op的类型进行总结
        
        第一类：原子数值比较 (Atomic Numeric) —— **快路径 (Fast Path)**
        
        | **助记符 (JSON)** | **枚举值 (Proto)** | **含义**                     | **典型应用场景**                         |
        | ----------------- | ------------------ | ---------------------------- | ---------------------------------------- |
        | **`EQ`**          | `0x01`             | Equal (`==`)                 | 版本号检查 (CAS)、状态机状态 (Status==1) |
        | **`NEQ`**         | `0x02`             | Not Equal (`!=`)             | 确保状态已变更                           |
        | **`GT`**          | `0x03`             | Greater Than (`>`)           | 超限检查                                 |
        | **`GTE`**         | `0x04`             | Greater Than or Equal (`>=`) | **最常用**：余额充足、库存充足检查       |
        | **`LT`**          | `0x05`             | Less Than (`<`)              | 限额检查                                 |
        | **`LTE`**         | `0x06`             | Less Than or Equal (`<=`)    | 限额检查                                 |
        
        第二类：位逻辑检查 (Bitwise Logic) —— **快路径 (Fast Path)**
        
        | **助记符 (JSON)** | **枚举值 (Proto)** | **含义**                                    | **典型应用场景**                      |
        | ----------------- | ------------------ | ------------------------------------------- | ------------------------------------- |
        | **`BIT_ANY`**     | `0x10`             | Any Bit Match (`(Val & Target) > 0`)        | 只要有任意一个权限即可通过 (OR逻辑)   |
        | **`BIT_ALL`**     | `0x11`             | All Bits Match (`(Val & Target) == Target`) | 必须拥有所有指定权限 (AND逻辑)        |
        | **`BIT_NONE`**    | `0x12`             | No Bits Match (`(Val & Target) == 0`)       | 必须没有某些标记 (如不能是黑名单用户) |
        
        第三类：复杂扩展 (Complex Extension) —— **慢路径 (Slow Path)**
        
        | **助记符 (JSON)** | **枚举值 (Proto)** | **含义**         | **典型应用场景**                             |
        | ----------------- | ------------------ | ---------------- | -------------------------------------------- |
        | **`EXPR`**        | `0xFF`             | Expression (RPN) | 混合计算、动态输入参数校验 (`A + Input < B`) |
    
* **Delta (增量)**: 在之前Guard所有判断通过后，可以对相关合约状态执行的增量操作。例如其他模型的合约状态修改都是 apple -=1，而我们则是在代理合约中得到 apple - 1，然后在主合约中进行再次验证，最终修改状态。所以是从状态覆盖写优化到了状态增量聚合。但是因为不是所有的操作都适合增量，所以我们会引入一些SET覆盖写的操作，例如 owner = Bob。

    * JOYUE 提倡 **“聚合写”** 而非覆盖写，但在特定场景下兼容 CAS 覆盖。

    * 以下是delta的数据结构示例
    
        ```json
        "deltas": [
            // 动作：直接覆盖为"已发货"状态
            { "key": "OrderStatus_1024", "op": "SET", "val": "1" },
          	{ "key": "Stock_iPhone","op": "SUB","val": "1"},
            {
              "key": "UserFlags_Alice",
              "op": "BIT_OR",  // OR 操作：0变1，1还是1 (激活)
              "val": "2"       // 二进制 010，对应第2位
            }
         ]
        ```

        这里我们对Delta中的op进行总结

        1. 第一类：算术增量 

           满足交换律，不需要锁，不需要关心当前值的具体版本。
    
           | **助记符 (JSON)** | **枚举值 (Proto)** | **含义** | **数学逻辑**   | **典型应用场景** |
           | ----------------- | ------------------ | -------- | -------------- | ---------------- |
           | **`ADD`**         | `0x01`             | Add      | `S' = S + Val` | 充值、增加库存   |
           | **`SUB`**         | `0x02`             | Subtract | `S' = S - Val` | 转账、扣库存     |

        2. 第二类：覆盖修改
    
           | **助记符 (JSON)** | **枚举 (Hex)** | **含义**   | **数学逻辑** | **典型应用场景**                                       |
           | ----------------- | -------------- | ---------- | ------------ | ------------------------------------------------------ |
           | **`SET`**         | `0x10`         | Set/Assign | `S' = Val`   | 状态机流转 (Pending->Paid)、更改所有者、开关 (Off->On) |

        3. 位操作

           修改状态的特定位，而不影响其他位。对于不同位的修改可并行。
    
           | **助记符 (JSON)** | **枚举 (Hex)** | **含义**   | **数学逻辑**    | **典型应用场景**                    |
           | ----------------- | -------------- | ---------- | --------------- | ----------------------------------- |
           | **`BIT_OR`**      | `0x20`         | Set Bits   | `S' = S | Val`  | **激活**: 开启功能、授予权限 (幂等) |
           | **`BIT_CLEAR`**   | `0x21`         | Unset Bits | `S' = S & ~Val` | **清除**: 关闭功能、移除权限 (幂等) |
           | **`BIT_XOR`**     | `0x22`         | Toggle     | `S' = S ^ Val`  | **翻转**: 切换开关、无锁交换数据    |

        4. 副作用 (Side Effects) —— **非状态修改**

           不修改 KV 状态，只产生追加数据。
    
           | **助记符 (JSON)** | **枚举 (Hex)** | **含义**   | **逻辑**         | **典型应用场景**            |
           | ----------------- | -------------- | ---------- | ---------------- | --------------------------- |
           | **`BYTES_APP`**   | `0x30`         | Append     | `S' = S + Bytes` | 追加非结构化数据 (如记事本) |
           | **`LOG`**         | `0xFF`         | Emit Event | `Emit(Key, Val)` | 抛出链上事件 (Event Logs)   |
    

### 5.2. 主合约执行流水线

主合约处理交易的核心在于 **“快速验证 + 异常重算”** 。当主合约A所在分片的Leader节点 master A收到交易意图时：

1.  **流程**:
    * **上下文加载**: 加载本地权威状态（跨分片数据通过 RPC 预取）。
    *  `Guard`校验。
      * 这里我们判断的步骤如下
        * 判断Guard集中使用的数据是否过期，如果过期则用masterA中的最新的数据进行Guard校验
        * 如果Guard验证通过，则认为该交易的Delta有效可以进入到下一个阶段
        * 如果Guard验证不通过，则说明可能进入到了其他分支，则利用逻辑树来进行重试，最后更新该交易的Delta集。

### 5.3. 矩阵式二阶段提交 (Matrix 2PC)
解决批量跨分片事务原子性。通过之前的处理，我们可以在一次代理合约的提交中得到同一个合约的多次操作来进行后续的批量处理。下面是一个水果商店的例子例子，当前在ShardA中合约A的状态为apple = 10，banana = 8，在ShardB中的合约B的用户余额状态，{user_1 = 10,user_2 = 50,user_3 = 50}，在ShardC中合约C中没有相关状态所以需要新加

|      |  用户  | 1 主合约A（本地） | 2 合约B（钱包）  Shard B | 3 合约C （积分）  Shard C | 4 状态  |
| ---- | :----: | ----------------- | ------------------------ | ------------------------- | ------- |
| Tx_1 | user_1 | Banana - 1        | user_1.bal - 10          | user_1.point + 1          | pending |
| Tx_2 | user_1 | Apple - 1         | user_1.bal - 20          | user_1.point + 5          | pending |
| Tx_3 | user_3 | Banana - 1        | user_3.bal - 10          | user_3.point + 1          | pending |
| Tx_4 | user_4 | Apple - 1         | user_4.bal - 20          | user_4.point + 5          | pending |

* **阶段一: 列式分发 (Scatter)**

    * **分发**: Master A 将矩阵按 **列 (Column)** 切分，将包含 Guard/Delta 的列数据包发送至 Master B 和 C。
    * **瞬时执行**: Master B/C 收到数据后，维护一个 **瞬时状态 (Transient State)**，串行执行校验。
        - **批次内依赖**: Tx_2 的校验基于 Tx_1 执行后的瞬时状态。
        - **软锁定 (Soft Freeze)**: 对涉及的资源进行预留（非最终提交），防止双花。
    * **版本校验策略**:
        - **版本弱相关**: 如余额扣减。若主合约版本V_1过期，但当前版本V_2余额仍充足，则校验 **Pass**。
        - **版本强相关**: 如状态机流转。若版本不一致，直接校验 **Fail**。
    * Master B 返回结果向量 `[OK, FAIL, OK, OK]`。Master C 返回结果向量 `[OK, OK, OK, OK]`，以及合约的最新状态信息

* **阶段二: 行式决策 (Gather & Decide)**
  
    * Master A 汇总各分片返回的向量，对每一行（即每一笔交易）进行决策：
    
      |      |  用户  | 1 主合约A（本地） | 2 合约B（钱包）  Shard B | 3 合约C （积分）  Shard C | 4 状态  |
      | ---- | :----: | :---------------: | :----------------------: | :-----------------------: | :-----: |
      | Tx_1 | user_1 |        OK         |            OK            |            OK             | pending |
      | Tx_2 | user_1 |        OK         |           FAIL           |            OK             | pending |
      | Tx_3 | user_3 |        OK         |            OK            |            OK             | pending |
      | Tx_4 | user_4 |        OK         |            OK            |            OK             | pending |
    
    * **全通过**: 状态置为 `COMMIT`。
    
    * **存在 FAIL**:
    
      1. **影响性评估**: 判断该 FAIL 是否导致原 Guard 逻辑失效。
      2. **逻辑重试 **: 利用 Master B 返回的**最新状态**，在 Master A 本地重跑当前交易的逻辑树。
      3. **决策生成**:
         - 若重算成功，生成新意图，状态置为 `RETRY` (需解冻旧状态，冻结新状态)。
         - 若重算失败，状态置为 `ROLLBACK` (回滚所有分片的预留)。
    
* **阶段三: 终局**
  
    * **多轮收敛**: Master A 将 `COMMIT` / `ROLLBACK` / `RETRY` 指令再次分发。对于 `RETRY` 的交易，重复阶段一和二。
    * **最终一致**: 当达到收敛阈值或所有交易终结后，状态落盘。
    * **状态扩散**: 通过 P2P 网络将最新状态广播至全网，更新代理合约的缓存。

## 6.开发接口与数据结构规范

为了弥合上层业务逻辑与底层分片架构的差异，JOYUE 定义了两层数据标准：底层使用 **Wire Format (Go)** 进行跨节点的高效传输与校验，上层提供 **JoyueLib SDK (Solidity)** 供开发者编写业务逻辑。

### 6.1.底层通信协议 (Wire Format - Go)

在网络层，为了支持主合约在异常发生时进行 **全局逻辑重算 (Global Logic Retry)**，交易不被物理拆分为碎片的意图，而是作为一个完整的 **“统一意图包” (Unified Intent Bundle)** 存在。

#### 6.1.1.顶层事务结构

```go
// MatrixTransaction 对应一笔用户签名的原始交易
type MatrixTransaction struct {
    TxID      [32]byte 
    Sender    [20]byte
    Nonce     uint64
    Signature []byte

    // 核心设计：统一意图包
    // 不在此处进行物理分片，而是保留交易的完整上下文，
    // 以便主合约在 LogicTree 重算时拥有全部输入数据。
    Intent    *UnifiedIntent 
}

// UnifiedIntent：包含了这笔交易在全网的所有“预判”和“动作”
type UnifiedIntent struct {
    // 1. 原始请求上下文 (重试基石)
    // 当 Guard 失败时，Master 使用 FunctionSelector + Args + 所有的 Guard 最新值
    // 在本地重新运行逻辑树，生成新的 Intent
    RawRequest struct {
        TargetAddr       [20]byte // 主合约地址 (入口)
        FunctionSelector [4]byte
        Args             []byte   // 原始输入参数 (msg.data)
    }

    // 2. 全局 Guard 列表 (线性排列)
    // 包含涉及全网所有分片的所有检查条件
    Guards []Guard

    // 3. 全局 Delta 列表 (线性排列)
    // 包含涉及全网所有分片的所有状态修改
    Deltas []Delta
}
```

#### 6.1.2.原子结构 (带路由元数据)

由于意图是统一列表，我们需要在每个原子操作中标识其归属，以便执行层进行路由。

```go
type Guard struct {
    // 路由标识：标识该 Guard 属于哪个合约（分片）的数据
    ContractAddr [20]byte `json:"addr"` 

    Key          []byte   `json:"key"`
    
    // 版本控制 (MVCC)
    // 代理合约生成意图时读取到的本地缓存版本号
    ReadVersion  uint64   `json:"r_ver"` 

    // 校验策略 
    // 0x00 (STRICT): 强一致 (CurrentVer == ReadVer)
    // 0x01 (RELAXED): 弱一致 (CurrentVer > ReadVer 且 Val 满足条件则通过)
    Strategy     uint8    `json:"strategy"`

    Op           OpType   `json:"op"`  // GT, EQ, EXPR...
    Val          []byte   `json:"val"` 
}

type Delta struct {
    // 路由标识
    ContractAddr [20]byte `json:"addr"`

    Key          []byte   `json:"key"`
    Op           OpType   `json:"op"`
    Val          []byte   `json:"val"`
}
```

### 6.2. 统一意图与重试机制的工作流

采用“统一意图”结构使得 **生成** 与 **执行** 彻底解耦，同时保证了 **Matrix 2PC** 能够获得完整的分片执行指令：

1. **生成阶段 (Client/Agent)**

   - 代理节点基于缓存执行逻辑，生成一个包含 `RawRequest`（原始参数）、全网所有 `Guards` 和全网所有 `Deltas` 的 **UnifiedIntent (统一意图包)**。
   - 该包保持了交易的上下文完整性，是后续主合约进行全局重算的基石。

2. **分发阶段 (Matrix Scatter)**

   - 主合约 (Master A) 收到统一意图包后，执行 **“列式切分”**。
   - 它遍历 `Guards` 和 `Deltas` 列表，根据 `ContractAddr` 将它们归类。
   - **发送列数据**: 主合约将包含相关 `Guard` **以及** 相关 `Delta` 的数据包（即矩阵的一列）发送给对应的分片 (Shard B, Shard C...)。
     - *Shard B 收到: `{ Guard_B (余额充足?), Delta_B (余额-10) }`*
     - *Shard C 收到: `{ Guard_C (无), Delta_C (积分+10) }`**

3. **瞬时执行与验证 (Transient Execution)**

   - 各分片 Leader (Master B/C) 收到列数据后：
     - 先验证 `Guard` (检查版本或数值)。
     - 若通过，则尝试应用 `Delta` 到瞬时状态，并预留资源 (Soft Freeze)。

   - 返回结果向量：`OK` 或 `FAIL` (附带最新状态)。

4. **重试阶段 (Logic Tree Retry)**

   - 若 Shard B 返回 `FAIL` (例如：余额不足，并返回了最新余额 `Balance_B'`)。
     - Master A 此时并未失败，而是启动 **惰性纠错**：
       - 利用 `UnifiedIntent.RawRequest` (原始参数) + `Balance_B'` (最新状态) 重新灌入本地的 **LogicTree**。
       - 逻辑树根据最新状态重新模拟执行流。

   - **重生成**: 逻辑树输出全新的 `UnifiedIntent'` (例如：改为“扣减备用金”)。

   - Master A 使用新意图重新触发分发阶段。

### 6.3. 开发者工具包 (SDK - Solidity)

为了支持复杂的业务逻辑（特别是 `if-else` 分支），SDK 引入了 **“断言包装器”** 模式。这使得 Solidity 在执行控制流的同时，能够自动捕获并生成对应的路径约束 (Guard)。

#### 6.3.1. SDK 核心结构

```solidity
library JoyueLib {
    // JVar: 状态变量包装
    struct JVar {
        address contractAddr;
        bytes32 key;
        bytes value;     // 缓存值
        uint64 version;  // 缓存版本
    }

    // Context: 意图构建上下文
    struct Context {
        Guard[] guards;
        Delta[] deltas;
        // ... (其他字段保持不变)
    }
    
    // RPN 构建器 (用于复杂条件)
    struct ExprBuilder {
        bytes[] tokens; // 暂存操作符和操作数
    }
}
```

#### 6.3.2. 基础断言 (Asserts)

用于“必须通过”的检查。。

- `var.assertGt(ctx, val)`: 生成 `GTE` Guard。
- `var.assertEq(ctx, val)`: 生成 `EQ` Guard (Strict Mode)。

#### 6.3.3. 条件分支 (Conditionals) - **新增核心功能**

在处理 `if-else` 时，我们需要根据运行时缓存的实际值，动态生成 **正向** 或 **反向** 的 Guard。

1. 简单条件分支

   ```solidity
   // SDK 方法定义 (伪代码)
   // 返回: (bool result) - 供 Solidity if 使用
   // 副作用: 向 ctx.guards 追加一个 Guard (GT 或 LTE)
   function checkGt(JVar memory var, Context memory ctx, uint256 threshold) internal pure returns (bool) {
       uint256 val = toUint(var.value);
       if (val > threshold) {
           // 实际路径：大于。生成 Guard: {Op: GT, Val: threshold}
           _addGuard(ctx, var, OP_GT, threshold);
           return true;
       } else {
           // 实际路径：不大于。生成反向 Guard: {Op: LTE, Val: threshold}
           // 这一步至关重要：告诉 Master 我之所以走 else，是因为变量 <= 阈值
           _addGuard(ctx, var, OP_LTE, threshold);
           return false;
       }
   }
   ```

2. 复杂条件分支 (RPN 表达式)

   对于 `if (A + B > C)` 这种逻辑，我们使用 `ExprBuilder` 来构建计算栈，并在最后一步进行“分支定界”。

   ```solidity
   // SDK 方法定义
   function buildExpr(Context memory ctx) internal pure returns (ExprBuilder memory);
   // 终结方法：计算表达式结果，生成 EXPR Guard，并返回 bool
   function checkExprGt(ExprBuilder memory b, Context memory ctx, uint256 val) internal pure returns (bool);
   ```

#### 6.3.4. 业务逻辑编写示例

**场景**：电商大促逻辑。

1. **库存检查**：如果 `库存 > 0`，则购买；否则进入预售流程。
2. **VIP 折扣**：如果 `积分 + 余额 > 1000`，打 8 折；否则原价。

```solidity
import "./JoyueLib.sol";

contract Shop {
    using JoyueLib for JoyueLib.JVar;
    using JoyueLib for JoyueLib.Context;
    using JoyueLib for JoyueLib.ExprBuilder;

    function buy(uint256 productId) external {
        JoyueLib.Context memory ctx = JoyueLib.newContext();

        // 1. 加载变量
        JoyueLib.JVar memory stock = JoyueLib.load(address(this), "stock");
        JoyueLib.JVar memory points = JoyueLib.load(address(this), "points");
        JoyueLib.JVar memory balance = JoyueLib.load(address(this), "balance");

        // 2. 分支逻辑 I：库存检查 (简单分支)
        // checkGt 会自动根据当前缓存值生成 Guard(Stock > 0) 或 Guard(Stock <= 0)
        if (stock.checkGt(ctx, 0)) {
            // --- 分支 A: 有库存 ---
            stock.sub(ctx, 1);
            
            // 3. 分支逻辑 II：VIP 检查 (复杂表达式分支)
            // 构建表达式: points + balance
            bool isVip = ctx.expr()
                .push(points)
                .push(balance)
                .op(OP_ADD)
                .checkGt(1000); // 终结：生成对应的 EXPR Guard

            if (isVip) {
                 // VIP 价格扣减
                 balance.sub(ctx, 80); 
            } else {
                 // 原价扣减
                 balance.sub(ctx, 100);
            }
            
        } else {
            // --- 分支 B: 无库存 (预售) ---
            // 此时 Intent 中已自动包含 Guard(Stock <= 0)
            // Master 重试时，如果发现 Stock 变成 0 了，就会正确引导进这个分支
            JoyueLib.JVar memory preOrder = JoyueLib.load(address(this), "pre_order_count");
            preOrder.add(ctx, 1);
        }

        // 4. 提交意图
        JoyueLib.emitIntent(ctx);
    }
}
```

