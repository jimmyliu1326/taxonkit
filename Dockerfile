FROM mambaorg/micromamba:0.14.0
ARG version=0.14.2
ARG package=taxonkit

RUN apt-get update && \
    apt-get install -y procps && \
    rm -rf /var/lib/apt/lists/* && \
    CONDA_DIR="/opt/conda" && \
    micromamba install -n base -y -c bioconda -c conda-forge ${package}=${version} && \
    micromamba clean --all --yes && \
    rm -rf $CONDA_DIR/conda-meta && \
    rm -rf $CONDA_DIR/include && \
    rm -rf $CONDA_DIR/lib/python3.*/site-packages/pip && \
    find $CONDA_DIR -name '__pycache__' -type d -exec rm -rf '{}' '+'

RUN mkdir -p /taxonkit_db && \
    wget -c ftp://ftp.ncbi.nih.gov/pub/taxonomy/taxdump.tar.gz -O /taxonkit_db/taxdump.tar.gz 

RUN cd /taxonkit_db && \
    tar -zxvf taxdump.tar.gz

ENV TAXONKIT_DB="/taxonkit_db"