// workflow_ml_training.go — ML 训练 & 大模型微调团队工作流。
//
// 参考:
//   - AutoML-Agent (PMLR v267): 多智能体全流程 AutoML
//   - Centaur (arXiv:2603.24647): 混合 HPO (经典优化器+LLM)
//   - FeRG-LLM (NAACL 2025): 表格数据特征工程
//   - DSPy (Stanford): 声明式 ML 管线
//   - On-Policy Distillation (arXiv:2604.00626)
//   - LoRA/QLoRA/DoRA: 参数高效微调
//
// 执行流程:
//   数据分析 ∥ 特征工程 → 模型设计 → 训练执行 → 评估分析 → 优化/蒸馏 → 综合报告
package agent

func mlTrainingWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "ml-training",
		Description: "ML训练&微调: 数据分析→特征工程→模型设计→训练→评估→蒸馏→报告",
		Mode:        "pipeline",
		Stages: []StageDef{
			{
				Name: "data-analysis", Role: "data-analyst",
				Prompt: `你是**资深数据科学家** (参考 AutoML-Agent 数据分析模块)。

ML任务: {objective}

## 数据分析任务
1. **数据理解**: 分析数据集结构、字段含义、数据类型
2. **统计分析**: 描述性统计、分布分析、相关性矩阵
3. **质量评估**: 缺失值比例、异常值检测、数据平衡性
4. **可视化建议**: 建议关键的 EDA 可视化图表

## 输出要求
- 使用 Python (pandas/numpy/matplotlib) 编写完整的分析代码
- 输出结构化的数据质量报告
- 给出数据预处理建议 (缺失值填充策略、编码方式等)
- 使用 WebSearch 搜索相关领域的数据处理最佳实践

## 代码要求
所有代码必须可直接运行, 包含完整的 import 和数据加载逻辑。`,
				Parallel: true,
			},
			{
				Name: "feature-engineering", Role: "ml-engineer",
				Prompt: `你是**特征工程专家** (参考 FeRG-LLM + LLM-FE: LLM驱动的特征生成与选择)。

ML任务: {objective}

## 特征工程任务
1. **特征生成**: 基于领域知识生成新特征 (交叉特征、聚合特征、时间特征等)
2. **特征选择**: 使用相关性分析、方差分析、互信息等方法选择特征
3. **特征转换**: 标准化、归一化、编码 (Label/One-Hot/Target Encoding)
4. **特征管线**: 用 sklearn Pipeline 封装整个预处理流程

## 输出要求
- 完整的 Python 特征工程代码 (sklearn Pipeline)
- 特征重要性分析报告
- 特征选择理由说明
- 建议的特征子集及预期效果

## 代码模板
使用 sklearn.pipeline.Pipeline + ColumnTransformer 封装。`,
				Parallel: true,
			},
			{
				Name: "model-design", Role: "ml-architect",
				DependsOn: []string{"data-analysis"},
				Prompt: `你是**ML架构师** (参考 AutoML-Agent 模型选择 + Centaur 混合HPO)。

ML任务: {objective}

数据分析结果:
{prev_result}

## 模型设计任务
1. **任务类型判断**: 分类/回归/聚类/时序/NLP/CV
2. **模型候选**: 根据数据特征推荐 3-5 个候选模型
   - 传统ML: XGBoost, LightGBM, CatBoost, RandomForest, SVM
   - 深度学习: MLP, CNN, LSTM, Transformer
   - 大模型微调: LoRA, QLoRA, full-finetune (若涉及LLM)
3. **超参搜索策略**: TPE / CMA-ES / Bayesian Optimization
4. **训练配置**: batch size, learning rate, epochs, early stopping

## 输出要求
- 模型选择决策矩阵 (每个模型的优劣对比表)
- 推荐的 Top 3 模型及理由
- 超参搜索空间定义 (Python dict)
- 训练配置文件
- 若涉及大模型微调: LoRA rank/alpha 建议, 量化策略

## 代码要求
输出完整的模型定义代码和超参搜索配置。`,
			},
			{
				Name: "training", Role: "ml-trainer",
				DependsOn: []string{"feature-engineering", "model-design"},
				Prompt: `你是**ML训练工程师** (参考 DSPy 声明式训练 + MLflow 实验追踪)。

ML任务: {objective}

特征工程和模型设计:
{prev_result}

## 训练执行任务
1. **训练脚本**: 基于前面的模型设计和特征管线, 编写完整的训练脚本
2. **交叉验证**: K-Fold 或时序分割验证
3. **实验追踪**: 集成 MLflow 或 W&B 记录实验参数和指标
4. **早停机制**: 基于验证集指标的早停策略
5. **模型保存**: 保存最佳模型和训练检查点

## 微调路径 (若涉及大模型)
- **LoRA 微调**: 使用 HuggingFace PEFT 库
- **QLoRA 微调**: 4-bit 量化 + LoRA
- **全参微调**: 对于小模型或关键场景

## 输出要求
- 完整可运行的训练脚本 (train.py)
- requirements.txt 依赖文件
- 训练日志模板
- 预期的训练时间和资源需求估算

## 代码要求
所有代码必须可直接运行。训练脚本包含完整的数据加载、训练循环、指标记录、模型保存。`,
			},
			{
				Name: "evaluation", Role: "ml-evaluator",
				DependsOn: []string{"training"},
				Prompt: `你是**模型评估专家** (参考 AutoGluon 评估框架)。

ML任务: {objective}

训练结果:
{prev_result}

## 评估任务
1. **指标计算**: 根据任务类型选择评估指标
   - 分类: Accuracy, Precision, Recall, F1, AUC-ROC, Confusion Matrix
   - 回归: MSE, RMSE, MAE, R², MAPE
   - 排序: NDCG, MAP
2. **错误分析**: 分析模型预测错误的模式
3. **公平性检查**: 检查模型在不同子群体上的表现差异
4. **可解释性**: SHAP / LIME / Feature Importance 分析
5. **对比分析**: 多模型之间的性能对比

## 输出要求
- 完整的评估代码和报告
- 评估指标对比表
- 关键错误案例分析
- 可解释性分析图表建议
- 模型是否达标的结论`,
			},
			{
				Name: "optimization-distillation", Role: "ml-optimizer",
				DependsOn: []string{"evaluation"},
				Prompt: `你是**模型优化与蒸馏专家** (参考 On-Policy Distillation + GaLore)。

ML任务: {objective}

评估结果:
{prev_result}

## 优化任务
1. **性能优化**: 推理速度、内存占用、批处理优化
2. **模型压缩**: 剪枝、量化 (INT8/INT4)、知识蒸馏
3. **蒸馏训练** (若涉及大模型):
   - Teacher-Student 架构设计
   - 蒸馏损失函数选择 (KL散度 / MSE / 组合)
   - 蒸馏训练配置
4. **部署准备**: ONNX 导出、TensorRT 优化、端侧部署

## 输出要求
- 优化后的模型代码
- 蒸馏训练脚本 (若适用)
- 优化前后的性能对比表
- 部署方案建议

## 代码要求
所有优化代码必须可运行, 包含性能基准测试。`,
			},
			{
				Name: "synthesis", Role: "synthesizer",
				DependsOn: []string{"optimization-distillation"},
				Prompt: `你是**ML项目负责人**, 负责综合所有阶段产出, 撰写最终报告。

ML任务: {objective}

各阶段产出:
{prev_result}

## 综合报告要求
1. **项目概述**: 任务定义、数据概况、技术方案选择
2. **实验结果汇总表**:
   | 模型 | 准确率 | F1 | 训练时间 | 推理延迟 | 模型大小 |
   |------|--------|-----|---------|---------|---------|
3. **最佳模型推荐**: 明确推荐并说明理由
4. **优化效果**: 优化/蒸馏前后的性能对比
5. **代码交付清单**: 列出所有代码文件及用途
6. **部署建议**: 推荐的部署方案和资源需求
7. **改进方向**: 下一步优化建议

## 质量要求
- 所有代码文件必须可独立运行
- 报告必须包含量化指标
- 保存为独立 Markdown 文件`,
			},
		},
	}
}
